// Package mcpserver exposes the WhatsApp client as MCP tools over HTTP/SSE.
package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/flix/whatsapp-mcp/internal/store"
	"github.com/flix/whatsapp-mcp/internal/whatsapp"
)

type Deps struct {
	WA      *whatsapp.Client
	History *store.HistoryStore
}

// Build wires up every MCP tool this server exposes.
func Build(deps Deps) *server.MCPServer {
	s := server.NewMCPServer(
		"whatsapp-mcp",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithLogging(),
	)

	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Send a WhatsApp text message to a person or group."),
		mcp.WithString("to", mcp.Required(),
			mcp.Description("Recipient: a phone number with country code (e.g. 6281234567890), a full JID (e.g. 6281234567890@s.whatsapp.net), or a group JID (e.g. 123456-789@g.us).")),
		mcp.WithString("message", mcp.Required(), mcp.Description("The text message to send.")),
	), deps.sendMessage)

	s.AddTool(mcp.NewTool("list_chats",
		mcp.WithDescription("List WhatsApp chats (individual and group) ordered by most recent activity, with an optional name search."),
		mcp.WithString("query", mcp.Description("Optional case-insensitive substring to filter chats by name or JID. Leave empty to list all.")),
		mcp.WithNumber("limit", mcp.Description("Max chats to return (default 50, max 200).")),
		mcp.WithNumber("offset", mcp.Description("Number of chats to skip, for pagination (default 0).")),
	), deps.listChats)

	s.AddTool(mcp.NewTool("get_chat_history",
		mcp.WithDescription("Get recent messages for a specific chat, newest first. Use the 'before' parameter to page further back in time."),
		mcp.WithString("chat_jid", mcp.Required(), mcp.Description("The chat JID, as returned by list_chats (e.g. 6281234567890@s.whatsapp.net or 123456-789@g.us).")),
		mcp.WithNumber("limit", mcp.Description("Max messages to return (default 50, max 500).")),
		mcp.WithString("before", mcp.Description("RFC3339 timestamp; only return messages strictly before this time. Omit to get the most recent messages.")),
	), deps.getChatHistory)

	s.AddTool(mcp.NewTool("search_messages",
		mcp.WithDescription("Full-text substring search over stored message history, optionally scoped to one chat."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Text to search for.")),
		mcp.WithString("chat_jid", mcp.Description("Optional: restrict the search to this chat JID.")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 50, max 200).")),
	), deps.searchMessages)

	s.AddTool(mcp.NewTool("search_contacts",
		mcp.WithDescription("Search WhatsApp contacts by name or phone number."),
		mcp.WithString("query", mcp.Description("Substring to match against contact name or phone number. Leave empty to list all known contacts.")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 50).")),
	), deps.searchContacts)

	s.AddTool(mcp.NewTool("connection_status",
		mcp.WithDescription("Check whether this server currently has an active, paired WhatsApp session."),
	), deps.connectionStatus)

	return s
}

func (d Deps) sendMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	to, err := req.RequireString("to")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("message")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	jid, err := whatsapp.ResolveJID(to)
	if err != nil {
		return mcp.NewToolResultErrorf("invalid recipient %q: %v", to, err), nil
	}

	msgID, err := d.WA.SendText(ctx, jid, text)
	if err != nil {
		return mcp.NewToolResultErrorf("failed to send message: %v", err), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Message sent to %s (id: %s)", jid.String(), msgID)), nil
}

func (d Deps) listChats(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	limit := req.GetInt("limit", 50)
	offset := req.GetInt("offset", 0)

	chats, err := d.History.ListChats(ctx, query, limit, offset)
	if err != nil {
		return mcp.NewToolResultErrorf("failed to list chats: %v", err), nil
	}
	if len(chats) == 0 {
		return mcp.NewToolResultText("No chats found. If you just paired, WhatsApp may still be syncing history."), nil
	}

	var b strings.Builder
	for _, c := range chats {
		kind := "individual"
		if c.IsGroup {
			kind = "group"
		}
		name := c.Name
		if name == "" {
			name = c.JID
		}
		fmt.Fprintf(&b, "- %s [%s] (%s)\n  last: %s — %q\n",
			name, kind, c.JID,
			c.LastMessageTime.Format(time.RFC3339), truncate(c.LastMessageText, 80))
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (d Deps) getChatHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatJID, err := req.RequireString("chat_jid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	limit := req.GetInt("limit", 50)

	var before time.Time
	if s := req.GetString("before", ""); s != "" {
		before, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return mcp.NewToolResultErrorf("invalid 'before' timestamp %q, expected RFC3339: %v", s, err), nil
		}
	}

	msgs, err := d.History.GetMessages(ctx, chatJID, limit, before)
	if err != nil {
		return mcp.NewToolResultErrorf("failed to get chat history: %v", err), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultText("No messages found for this chat."), nil
	}

	var b strings.Builder
	// Print oldest-first within the page so it reads like a conversation.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		who := m.SenderName
		if m.FromMe {
			who = "me"
		} else if who == "" {
			who = m.SenderJID
		}
		body := m.Text
		if body == "" && m.MediaType != "" {
			body = "[" + m.MediaType + "]"
			if m.Caption != "" {
				body += " " + m.Caption
			}
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", m.Timestamp.Format(time.RFC3339), who, body)
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (d Deps) searchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	chatJID := req.GetString("chat_jid", "")
	limit := req.GetInt("limit", 50)

	msgs, err := d.History.SearchMessages(ctx, query, chatJID, limit)
	if err != nil {
		return mcp.NewToolResultErrorf("search failed: %v", err), nil
	}
	if len(msgs) == 0 {
		return mcp.NewToolResultText("No matching messages found."), nil
	}

	var b strings.Builder
	for _, m := range msgs {
		who := m.SenderName
		if m.FromMe {
			who = "me"
		}
		fmt.Fprintf(&b, "[%s] chat=%s %s: %s\n", m.Timestamp.Format(time.RFC3339), m.ChatJID, who, m.Text)
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (d Deps) searchContacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	limit := req.GetInt("limit", 50)

	contacts, err := d.WA.SearchContacts(ctx, query, limit)
	if err != nil {
		return mcp.NewToolResultErrorf("failed to search contacts: %v", err), nil
	}
	if len(contacts) == 0 {
		return mcp.NewToolResultText("No matching contacts found."), nil
	}

	var b strings.Builder
	for _, c := range contacts {
		name := c.Name
		if name == "" {
			name = "(no name)"
		}
		fmt.Fprintf(&b, "- %s — %s (%s)\n", name, c.PhoneNumber, c.JID)
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (d Deps) connectionStatus(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if d.WA.IsPaired() {
		return mcp.NewToolResultText("Connected: WhatsApp session is active."), nil
	}
	msg := "Not paired: waiting for QR code scan."
	if code := d.WA.PendingQRCode(); code != "" {
		msg += " Visit the server's /qr endpoint to scan it."
	}
	return mcp.NewToolResultText(msg), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// AuthMiddleware enforces a static bearer token on every HTTP request when
// token is non-empty. Pass "" to disable.
func AuthMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
