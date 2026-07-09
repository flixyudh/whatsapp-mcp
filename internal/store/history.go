// Package store persists chat and message history in SQLite.
//
// This is deliberately separate from whatsmeow's own sqlstore (which only
// holds session/device crypto material). whatsmeow does not keep a
// queryable message history itself -- it only emits events -- so we record
// every inbound/outbound message here to answer MCP "get history" calls.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Chat struct {
	JID             string
	Name            string
	IsGroup         bool
	LastMessageTime time.Time
	LastMessageText string
	UnreadCount     int
}

type Message struct {
	ID         string
	ChatJID    string
	SenderJID  string
	SenderName string
	Timestamp  time.Time
	Text       string
	FromMe     bool
	MediaType  string // "", "image", "video", "audio", "document", "sticker"
	Caption    string
}

type HistoryStore struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*HistoryStore, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}
	db.SetMaxOpenConns(1) // sqlite + WAL is happiest with a single writer connection

	s := &HistoryStore{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *HistoryStore) Close() error {
	return s.db.Close()
}

func (s *HistoryStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS chats (
	jid TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	is_group INTEGER NOT NULL DEFAULT 0,
	last_message_time INTEGER NOT NULL DEFAULT 0,
	last_message_text TEXT NOT NULL DEFAULT '',
	unread_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
	id TEXT NOT NULL,
	chat_jid TEXT NOT NULL,
	sender_jid TEXT NOT NULL DEFAULT '',
	sender_name TEXT NOT NULL DEFAULT '',
	timestamp INTEGER NOT NULL,
	text TEXT NOT NULL DEFAULT '',
	from_me INTEGER NOT NULL DEFAULT 0,
	media_type TEXT NOT NULL DEFAULT '',
	caption TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (id, chat_jid)
);

CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_jid, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_chats_last_time ON chats(last_message_time DESC);
`)
	if err != nil {
		return fmt.Errorf("migrate history db: %w", err)
	}
	return nil
}

// UpsertChat inserts or updates a chat's metadata. Empty name is ignored on update
// so we don't clobber a previously-known display name with a blank one.
func (s *HistoryStore) UpsertChat(ctx context.Context, c Chat) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO chats (jid, name, is_group, last_message_time, last_message_text, unread_count)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(jid) DO UPDATE SET
	name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
	is_group = excluded.is_group,
	last_message_time = MAX(chats.last_message_time, excluded.last_message_time),
	last_message_text = CASE WHEN excluded.last_message_time >= chats.last_message_time THEN excluded.last_message_text ELSE chats.last_message_text END
`, c.JID, c.Name, boolToInt(c.IsGroup), c.LastMessageTime.Unix(), c.LastMessageText, c.UnreadCount)
	return err
}

// SaveMessage inserts a message, ignoring duplicates (idempotent on (id, chat_jid)).
func (s *HistoryStore) SaveMessage(ctx context.Context, m Message) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO messages (id, chat_jid, sender_jid, sender_name, timestamp, text, from_me, media_type, caption)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id, chat_jid) DO NOTHING
`, m.ID, m.ChatJID, m.SenderJID, m.SenderName, m.Timestamp.Unix(), m.Text, boolToInt(m.FromMe), m.MediaType, m.Caption)
	return err
}

// ListChats returns chats ordered by most recent activity, optionally filtered
// by a case-insensitive substring match on name or JID.
func (s *HistoryStore) ListChats(ctx context.Context, query string, limit, offset int) ([]Chat, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT jid, name, is_group, last_message_time, last_message_text, unread_count
FROM chats
WHERE (? = '' OR name LIKE '%' || ? || '%' OR jid LIKE '%' || ? || '%')
ORDER BY last_message_time DESC
LIMIT ? OFFSET ?
`, query, query, query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Chat
	for rows.Next() {
		var c Chat
		var isGroup int
		var lastTS int64
		if err := rows.Scan(&c.JID, &c.Name, &isGroup, &lastTS, &c.LastMessageText, &c.UnreadCount); err != nil {
			return nil, err
		}
		c.IsGroup = isGroup != 0
		c.LastMessageTime = time.Unix(lastTS, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetMessages returns messages for a chat, newest first, optionally paginated
// with beforeTimestamp (exclusive) for "load older messages" style pagination.
func (s *HistoryStore) GetMessages(ctx context.Context, chatJID string, limit int, beforeTimestamp time.Time) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	before := int64(1 << 62)
	if !beforeTimestamp.IsZero() {
		before = beforeTimestamp.Unix()
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT id, chat_jid, sender_jid, sender_name, timestamp, text, from_me, media_type, caption
FROM messages
WHERE chat_jid = ? AND timestamp < ?
ORDER BY timestamp DESC
LIMIT ?
`, chatJID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var ts int64
		var fromMe int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &ts, &m.Text, &fromMe, &m.MediaType, &m.Caption); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.FromMe = fromMe != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchMessages does a substring search over message text across all chats
// (or a single chat if chatJID is non-empty).
func (s *HistoryStore) SearchMessages(ctx context.Context, query, chatJID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, chat_jid, sender_jid, sender_name, timestamp, text, from_me, media_type, caption
FROM messages
WHERE text LIKE '%' || ? || '%' AND (? = '' OR chat_jid = ?)
ORDER BY timestamp DESC
LIMIT ?
`, query, chatJID, chatJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var ts int64
		var fromMe int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &ts, &m.Text, &fromMe, &m.MediaType, &m.Caption); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.FromMe = fromMe != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
