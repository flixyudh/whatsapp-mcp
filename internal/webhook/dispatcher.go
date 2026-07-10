// Package webhook dispatches incoming WhatsApp messages to an external HTTP
// endpoint (e.g. an n8n Webhook trigger node), optionally filtered to a set
// of sender phone numbers.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/flix/whatsapp-mcp/internal/store"
)

// Dispatcher POSTs a JSON payload for each incoming message that matches its
// sender filter. A zero-value Dispatcher (empty URL) is inert: Dispatch
// becomes a no-op, so callers don't need to nil-check.
type Dispatcher struct {
	url    string
	secret string
	allow  map[string]struct{} // normalized phone numbers, digits only; nil/empty = allow all
	client *http.Client
}

// New builds a Dispatcher. url may be empty to disable dispatch entirely.
// fromNumbers restricts dispatch to messages sent by these numbers (digits
// only, e.g. "6281234567890" -- '+', spaces, and dashes are stripped
// automatically); empty/nil means "dispatch for every incoming message".
func New(url string, fromNumbers []string, secret string) *Dispatcher {
	var allow map[string]struct{}
	if len(fromNumbers) > 0 {
		allow = make(map[string]struct{}, len(fromNumbers))
		for _, n := range fromNumbers {
			if norm := normalizeNumber(n); norm != "" {
				allow[norm] = struct{}{}
			}
		}
	}
	return &Dispatcher{
		url:    strings.TrimSpace(url),
		secret: secret,
		allow:  allow,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Payload is the JSON body POSTed to the webhook URL.
type Payload struct {
	ID         string `json:"id"`
	ChatJID    string `json:"chat_jid"`
	SenderJID  string `json:"sender_jid"`
	SenderName string `json:"sender_name"`
	Timestamp  string `json:"timestamp"` // RFC3339
	Text       string `json:"text"`
	MediaType  string `json:"media_type,omitempty"`
	Caption    string `json:"caption,omitempty"`
	IsGroup    bool   `json:"is_group"`
}

// Dispatch checks the message against the sender filter and, if it matches,
// POSTs it to the configured URL in a background goroutine (so a slow or
// unreachable webhook receiver never blocks WhatsApp message handling).
// Safe to call on a nil *Dispatcher or one built with an empty URL.
func (d *Dispatcher) Dispatch(m store.Message, isGroup bool) {
	if d == nil || d.url == "" {
		return
	}
	if !d.matches(m.SenderJID) {
		return
	}

	payload := Payload{
		ID:         m.ID,
		ChatJID:    m.ChatJID,
		SenderJID:  m.SenderJID,
		SenderName: m.SenderName,
		Timestamp:  m.Timestamp.Format(time.RFC3339),
		Text:       m.Text,
		MediaType:  m.MediaType,
		Caption:    m.Caption,
		IsGroup:    isGroup,
	}

	go d.send(payload)
}

func (d *Dispatcher) matches(senderJID string) bool {
	if len(d.allow) == 0 {
		return true
	}
	// senderJID looks like "6281234567890@s.whatsapp.net" (or with a device
	// suffix, "...:12@s.whatsapp.net") -- compare just the number part.
	user, _, _ := strings.Cut(senderJID, "@")
	user, _, _ = strings.Cut(user, ":")
	_, ok := d.allow[normalizeNumber(user)]
	return ok
}

func (d *Dispatcher) send(payload Payload) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("webhook: failed to encode payload: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		log.Printf("webhook: failed to build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if d.secret != "" {
		req.Header.Set("X-Webhook-Signature", sign(body, d.secret))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		log.Printf("webhook: delivery to %s failed: %v", d.url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("webhook: delivery to %s returned status %d", d.url, resp.StatusCode)
	}
}

// sign returns "sha256=<hex hmac>", matching the convention used by GitHub
// and Stripe webhooks (easy to verify from an n8n Code node or Function node).
func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func normalizeNumber(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
