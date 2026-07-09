// Package whatsapp wraps go.mau.fi/whatsmeow to provide a small, MCP-friendly
// surface: connect/pair once via QR, then send messages and mirror every
// inbound/outbound message + history-sync backfill into the history store.
package whatsapp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/flix/whatsapp-mcp/internal/store"
)

// Client wraps a whatsmeow client plus our own history persistence.
type Client struct {
	wa      *whatsmeow.Client
	history *store.HistoryStore
	log     waLog.Logger

	mu         sync.RWMutex
	lastQRCode string
	paired     atomic.Bool
}

// New creates and connects the WhatsApp client. dbPath is where whatsmeow
// persists its own session/device (encryption keys, etc) -- keep this safe,
// losing it means re-scanning the QR code and whatsmeow forgetting the
// session. onQR is called every time a fresh QR code is issued during
// pairing (nil after pairing succeeds).
func New(ctx context.Context, dbPath string, history *store.HistoryStore, logLevel string, onQR func(code string)) (*Client, error) {
	dbLog := waLog.Stdout("Database", logLevel, true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+dbPath+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000", dbLog)
	if err != nil {
		return nil, fmt.Errorf("open whatsmeow session store: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}

	clientLog := waLog.Stdout("Client", logLevel, true)
	waClient := whatsmeow.NewClient(device, clientLog)

	c := &Client{wa: waClient, history: history, log: clientLog}
	waClient.AddEventHandler(c.handleEvent)

	if waClient.Store.ID == nil {
		// No session yet: need to pair via QR code.
		qrChan, _ := waClient.GetQRChannel(ctx)
		if err := waClient.Connect(); err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		go func() {
			for evt := range qrChan {
				switch evt.Event {
				case "code":
					c.mu.Lock()
					c.lastQRCode = evt.Code
					c.mu.Unlock()
					if onQR != nil {
						onQR(evt.Code)
					}
				case "success":
					c.paired.Store(true)
					c.mu.Lock()
					c.lastQRCode = ""
					c.mu.Unlock()
					c.log.Infof("Successfully paired with WhatsApp")
				case "timeout":
					c.log.Warnf("QR pairing timed out, generating a new code requires a server restart")
				}
			}
		}()
	} else {
		c.paired.Store(true)
		if err := waClient.Connect(); err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
	}

	return c, nil
}

func (c *Client) Close() {
	c.wa.Disconnect()
}

// IsPaired reports whether we have an active WhatsApp session (as opposed to
// waiting for a QR scan).
func (c *Client) IsPaired() bool {
	return c.paired.Load()
}

// PendingQRCode returns the most recent QR pairing code, or "" if already
// paired or a code has not been issued yet.
func (c *Client) PendingQRCode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastQRCode
}

// ResolveJID turns a phone number ("6281234567890"), a full JID
// ("6281234567890@s.whatsapp.net"), or a group JID ("123-456@g.us") into a
// types.JID. Bare digit strings are assumed to be individual contacts.
func ResolveJID(input string) (types.JID, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return types.JID{}, fmt.Errorf("empty recipient")
	}
	if strings.Contains(input, "@") {
		return types.ParseJID(input)
	}
	// Strip anything that isn't a digit (spaces, +, dashes) for convenience.
	var digits strings.Builder
	for _, r := range input {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	if digits.Len() == 0 {
		return types.JID{}, fmt.Errorf("recipient %q is not a phone number or JID", input)
	}
	return types.NewJID(digits.String(), types.DefaultUserServer), nil
}

// SendText sends a plain text message and returns the outbound message ID.
func (c *Client) SendText(ctx context.Context, to types.JID, text string) (string, error) {
	if !c.IsPaired() {
		return "", fmt.Errorf("whatsapp session not paired yet: scan the QR code first")
	}
	resp, err := c.wa.SendMessage(ctx, to, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}

	// Mirror our own outbound message into history immediately so
	// get_chat_history reflects it without waiting for the echo event.
	_ = c.history.UpsertChat(ctx, store.Chat{
		JID:             to.String(),
		IsGroup:         to.Server == types.GroupServer,
		LastMessageTime: resp.Timestamp,
		LastMessageText: text,
	})
	_ = c.history.SaveMessage(ctx, store.Message{
		ID:        resp.ID,
		ChatJID:   to.String(),
		SenderJID: c.wa.Store.ID.String(),
		Timestamp: resp.Timestamp,
		Text:      text,
		FromMe:    true,
	})

	return resp.ID, nil
}

type Contact struct {
	JID         string
	Name        string
	PushName    string
	PhoneNumber string
}

// SearchContacts scans the local contact store (populated by WhatsApp as it
// syncs) for entries whose name or number contains the query.
func (c *Client) SearchContacts(ctx context.Context, query string, limit int) ([]Contact, error) {
	all, err := c.wa.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("read contacts: %w", err)
	}
	query = strings.ToLower(query)
	var out []Contact
	for jid, info := range all {
		name := info.FullName
		if name == "" {
			name = info.PushName
		}
		if query != "" && !strings.Contains(strings.ToLower(name), query) && !strings.Contains(jid.User, query) {
			continue
		}
		out = append(out, Contact{
			JID:         jid.String(),
			Name:        name,
			PushName:    info.PushName,
			PhoneNumber: jid.User,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// handleEvent is the single whatsmeow event callback: it fans out to the
// pieces we care about and mirrors relevant data into the history store.
func (c *Client) handleEvent(rawEvt interface{}) {
	ctx := context.Background()

	switch evt := rawEvt.(type) {
	case *events.Message:
		c.onMessage(ctx, evt)
	case *events.HistorySync:
		c.onHistorySync(ctx, evt)
	case *events.Connected:
		c.log.Infof("Connected to WhatsApp")
	case *events.LoggedOut:
		c.paired.Store(false)
		c.log.Warnf("Logged out of WhatsApp (reason: %v). Delete the session DB and restart to re-pair.", evt.Reason)
	}
}

func (c *Client) onMessage(ctx context.Context, evt *events.Message) {
	text, mediaType, caption := extractContent(evt.Message)
	if text == "" && mediaType == "" {
		return // reactions, receipts, protocol messages, etc: nothing to store
	}

	chatJID := evt.Info.Chat.String()
	senderName := evt.Info.PushName
	if evt.Info.IsFromMe {
		senderName = "me"
	}

	preview := text
	if preview == "" {
		preview = "[" + mediaType + "]"
	}

	_ = c.history.UpsertChat(ctx, store.Chat{
		JID:             chatJID,
		IsGroup:         evt.Info.IsGroup,
		LastMessageTime: evt.Info.Timestamp,
		LastMessageText: preview,
	})
	_ = c.history.SaveMessage(ctx, store.Message{
		ID:         evt.Info.ID,
		ChatJID:    chatJID,
		SenderJID:  evt.Info.Sender.String(),
		SenderName: senderName,
		Timestamp:  evt.Info.Timestamp,
		Text:       text,
		FromMe:     evt.Info.IsFromMe,
		MediaType:  mediaType,
		Caption:    caption,
	})
}

// onHistorySync backfills chat history that WhatsApp pushes shortly after
// pairing. The payload shape is intentionally handled defensively: it is an
// internal WhatsApp sync format that has changed before and may again.
func (c *Client) onHistorySync(ctx context.Context, evt *events.HistorySync) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Errorf("recovered while parsing history sync payload: %v", r)
		}
	}()

	if evt.Data == nil {
		return
	}
	for _, conv := range evt.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		name := conv.GetDisplayName()

		var lastTS time.Time
		var lastText string
		for _, hMsg := range conv.GetMessages() {
			webMsg := hMsg.GetMessage()
			if webMsg == nil || webMsg.GetKey() == nil {
				continue
			}
			text, mediaType, caption := extractContent(webMsg.GetMessage())
			if text == "" && mediaType == "" {
				continue
			}
			ts := time.Unix(int64(webMsg.GetMessageTimestamp()), 0)
			sender := chatJID.String()
			if p := webMsg.GetParticipant(); p != "" {
				sender = p
			}

			_ = c.history.SaveMessage(ctx, store.Message{
				ID:        webMsg.GetKey().GetID(),
				ChatJID:   chatJID.String(),
				SenderJID: sender,
				Timestamp: ts,
				Text:      text,
				FromMe:    webMsg.GetKey().GetFromMe(),
				MediaType: mediaType,
				Caption:   caption,
			})
			if ts.After(lastTS) {
				lastTS = ts
				lastText = text
				if lastText == "" {
					lastText = "[" + mediaType + "]"
				}
			}
		}

		_ = c.history.UpsertChat(ctx, store.Chat{
			JID:             chatJID.String(),
			Name:            name,
			IsGroup:         chatJID.Server == types.GroupServer,
			LastMessageTime: lastTS,
			LastMessageText: lastText,
		})
	}
}

// extractContent pulls a best-effort text/media summary out of a waE2E.Message,
// covering the common message types. Unsupported/unknown types return "".
func extractContent(msg *waProto.Message) (text, mediaType, caption string) {
	if msg == nil {
		return "", "", ""
	}
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation(), "", ""
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText(), "", ""
	case msg.GetImageMessage() != nil:
		return "", "image", msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return "", "video", msg.GetVideoMessage().GetCaption()
	case msg.GetAudioMessage() != nil:
		return "", "audio", ""
	case msg.GetDocumentMessage() != nil:
		return "", "document", msg.GetDocumentMessage().GetCaption()
	case msg.GetStickerMessage() != nil:
		return "", "sticker", ""
	default:
		return "", "", ""
	}
}
