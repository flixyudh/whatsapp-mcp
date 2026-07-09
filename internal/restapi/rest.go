// Package restapi exposes a small plain-JSON HTTP API alongside the MCP
// server, for callers that just want to make an HTTP call rather than speak
// the MCP protocol -- e.g. an n8n "HTTP Request" node at the end of a
// workflow, a shell script, curl, etc. It wraps the exact same WhatsApp
// client and history store the MCP tools use.
package restapi

import (
	"encoding/json"
	"net/http"

	"github.com/flix/whatsapp-mcp/internal/store"
	"github.com/flix/whatsapp-mcp/internal/whatsapp"
)

type Deps struct {
	WA      *whatsapp.Client
	History *store.HistoryStore
}

// Handler returns an http.Handler serving the REST API under whatever prefix
// it's mounted at (routes below are relative: "messages", "status", ...).
func Handler(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/messages", deps.sendMessage)
	mux.HandleFunc("GET /api/v1/status", deps.status)
	mux.HandleFunc("GET /api/v1/chats", deps.listChats)
	return mux
}

type sendMessageRequest struct {
	To      string `json:"to"`
	Message string `json:"message"`
}

type sendMessageResponse struct {
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	To     string `json:"to,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (d Deps) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, sendMessageResponse{Status: "error", Error: "invalid JSON body: " + err.Error()})
		return
	}
	if req.To == "" || req.Message == "" {
		writeJSON(w, http.StatusBadRequest, sendMessageResponse{Status: "error", Error: "both 'to' and 'message' are required"})
		return
	}

	jid, err := whatsapp.ResolveJID(req.To)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, sendMessageResponse{Status: "error", Error: "invalid 'to': " + err.Error()})
		return
	}

	msgID, err := d.WA.SendText(r.Context(), jid, req.Message)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, sendMessageResponse{Status: "error", Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, sendMessageResponse{Status: "sent", ID: msgID, To: jid.String()})
}

type statusResponse struct {
	Paired bool `json:"paired"`
}

func (d Deps) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Paired: d.WA.IsPaired()})
}

type chatResponse struct {
	JID             string `json:"jid"`
	Name            string `json:"name"`
	IsGroup         bool   `json:"is_group"`
	LastMessageTime string `json:"last_message_time"`
	LastMessageText string `json:"last_message_text"`
}

// listChats mirrors the list_chats MCP tool as plain JSON, e.g. for an n8n
// step that needs to resolve a chat name to a JID before sending.
// Query params: q (search), limit, offset.
func (d Deps) listChats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)

	chats, err := d.History.ListChats(r.Context(), q, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]chatResponse, 0, len(chats))
	for _, c := range chats {
		out = append(out, chatResponse{
			JID:             c.JID,
			Name:            c.Name,
			IsGroup:         c.IsGroup,
			LastMessageTime: c.LastMessageTime.Format("2006-01-02T15:04:05Z07:00"),
			LastMessageText: c.LastMessageText,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func atoiDefault(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	return n
}
