package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

type fakeAltResolver struct {
	alt map[string]types.JID
}

func (f fakeAltResolver) GetAltJID(ctx context.Context, jid types.JID) (types.JID, error) {
	return f.alt[jid.String()], nil
}

func TestResolveStoredChatJID(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("opening in-memory db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (id TEXT, chat_jid TEXT, PRIMARY KEY (id, chat_jid))`); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE jid_aliases (old_jid TEXT PRIMARY KEY, new_jid TEXT NOT NULL)`); err != nil {
		t.Fatalf("creating alias table: %v", err)
	}
	store := &MessageStore{db: db}

	pnKey := types.JID{User: "16465559999", Server: types.DefaultUserServer}
	lidKey := types.JID{User: "74616466886757", Server: types.HiddenUserServer}
	legacyKey := types.JID{User: "16465559999", Server: "c.us"}
	if _, err := db.Exec("INSERT INTO messages (id, chat_jid) VALUES ('MSG1', ?)", pnKey.String()); err != nil {
		t.Fatalf("inserting row: %v", err)
	}
	if _, err := db.Exec("INSERT INTO jid_aliases (old_jid, new_jid) VALUES (?, ?)", legacyKey.String(), pnKey.String()); err != nil {
		t.Fatalf("inserting alias: %v", err)
	}
	alts := fakeAltResolver{alt: map[string]types.JID{
		lidKey.String(): pnKey,
		pnKey.String():  lidKey,
	}}

	tests := []struct {
		name string
		key  types.JID
		want types.JID
	}{
		{"message under given key keeps the key even when an alt is mapped", pnKey, pnKey},
		{"stale alternate-namespace key resolves to the stored key", lidKey, pnKey},
		{"renamed key follows its jid_aliases row", legacyKey, pnKey},
		{"unknown key with no alt mapping is returned unchanged", types.JID{User: "19998887777", Server: types.DefaultUserServer}, types.JID{User: "19998887777", Server: types.DefaultUserServer}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveStoredChatJID(context.Background(), alts, store, "MSG1", tt.key); got != tt.want {
				t.Errorf("resolveStoredChatJID(%s) = %s, want %s", tt.key, got, tt.want)
			}
		})
	}
}

func TestBuildReplyContextInfo(t *testing.T) {
	participant := types.JID{User: "74616466886757", Server: types.HiddenUserServer}

	tests := []struct {
		name           string
		id             string
		content        string
		mediaType      string
		wantQuotedText string
	}{
		{"non-empty content is quoted verbatim", "ABCD1234", "hello there", "", "hello there"},
		{"image caption is quoted verbatim", "ABCD1235", "look at this", "image", "look at this"},
		{"empty image content quotes an image placeholder", "EFGH5678", "", "image", "[image]"},
		{"empty video content quotes a video placeholder", "EFGH5679", "", "video", "[video]"},
		{"empty audio content quotes an audio placeholder", "EFGH5680", "", "audio", "[audio]"},
		{"empty document content quotes a document placeholder", "EFGH5681", "", "document", "[document]"},
		{"empty content with no media type falls back to a generic placeholder", "EFGH5682", "", "", "[media]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildReplyContextInfo(tt.id, participant, tt.content, tt.mediaType)

			if got.GetStanzaID() != tt.id {
				t.Errorf("StanzaID = %q, want %q", got.GetStanzaID(), tt.id)
			}
			if got.GetParticipant() != participant.String() {
				t.Errorf("Participant = %q, want %q", got.GetParticipant(), participant.String())
			}
			if got.GetQuotedMessage().GetConversation() != tt.wantQuotedText {
				t.Errorf("QuotedMessage.Conversation = %q, want %q", got.GetQuotedMessage().GetConversation(), tt.wantQuotedText)
			}
		})
	}
}

func TestGetMessageForReply(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("opening in-memory db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (
		id TEXT, chat_jid TEXT, sender TEXT, content TEXT, is_from_me BOOLEAN, media_type TEXT,
		PRIMARY KEY (id, chat_jid)
	)`); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	store := &MessageStore{db: db}

	const msgID = "MSG1"
	chatA, chatB := "16465559999@s.whatsapp.net", "120363000000000000@g.us"
	for _, row := range []struct {
		chat, sender, content, mediaType string
		isFromMe                         bool
	}{
		{chatA, "16465559999", "hello from A", "", false},
		{chatB, "74616466886757@lid", "", "image", true},
	} {
		if _, err := db.Exec("INSERT INTO messages (id, chat_jid, sender, content, is_from_me, media_type) VALUES (?, ?, ?, ?, ?, ?)",
			msgID, row.chat, row.sender, row.content, row.isFromMe, row.mediaType); err != nil {
			t.Fatalf("inserting row: %v", err)
		}
	}

	sender, content, mediaType, isFromMe, err := store.GetMessageForReply(msgID, chatA)
	if err != nil || sender != "16465559999" || content != "hello from A" || mediaType != "" || isFromMe {
		t.Errorf("chat A lookup = (%q, %q, %q, %v, %v), want (16465559999, hello from A, \"\", false, nil)", sender, content, mediaType, isFromMe, err)
	}

	sender, content, mediaType, isFromMe, err = store.GetMessageForReply(msgID, chatB)
	if err != nil || sender != "74616466886757@lid" || content != "" || mediaType != "image" || !isFromMe {
		t.Errorf("chat B lookup = (%q, %q, %q, %v, %v), want (74616466886757@lid, \"\", image, true, nil)", sender, content, mediaType, isFromMe, err)
	}

	if _, _, _, _, err := store.GetMessageForReply(msgID, "19998887777@s.whatsapp.net"); err != sql.ErrNoRows {
		t.Errorf("lookup in uninvolved chat: err = %v, want sql.ErrNoRows", err)
	}
	if _, _, _, _, err := store.GetMessageForReply("", chatA); err != sql.ErrNoRows {
		t.Errorf("empty id lookup: err = %v, want sql.ErrNoRows", err)
	}
}

func TestPairDisplayName(t *testing.T) {
	tests := []struct {
		goos string
		want string
	}{
		{"darwin", "Chrome (Mac OS)"},
		{"windows", "Chrome (Windows)"},
		{"linux", "Chrome (Linux)"},
		{"freebsd", "Chrome (Linux)"},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			if got := pairDisplayName(tt.goos); got != tt.want {
				t.Errorf("pairDisplayName(%q) = %q, want %q", tt.goos, got, tt.want)
			}
		})
	}
}

// NewMessageStore's DDL is a concatenation of a literal and contactsSchema,
// and nothing else in the suite executes it. Pin that it still creates every
// table, and that the schema agrees with the statement that writes to it.
func TestNewMessageStoreSchema(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer store.Close()

	for _, table := range []string{"chats", "messages", "jid_aliases", "contacts", "group_events"} {
		var name string
		if err := store.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	if _, err := store.db.Exec(upsertContact, "1@s.whatsapp.net", "A", "B", "C"); err != nil {
		t.Errorf("upsert against the created schema: %v", err)
	}
}

// StoreChat must not erase a known name when a later message resolves to none,
// and must not delete the messages that reference the chat.
func TestStoreChatPreservesNameAndMessages(t *testing.T) {
	db := newContactTestDB(t)
	store := &MessageStore{db: db}
	jid := "15550006001@s.whatsapp.net"
	first := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)

	if err := store.StoreChat(jid, "Kate Sydney Cook", first); err != nil {
		t.Fatalf("first StoreChat: %v", err)
	}
	if _, err := db.Exec("INSERT INTO messages (id, chat_jid, content) VALUES (?, ?, ?)", "M1", jid, "hi"); err != nil {
		t.Fatalf("inserting message: %v", err)
	}

	// A later message whose contact lookup came up empty.
	if err := store.StoreChat(jid, "", second); err != nil {
		t.Fatalf("second StoreChat: %v", err)
	}
	// ...and one that resolved only to a phone number.
	if err := store.StoreChat(jid, "15550006001", second); err != nil {
		t.Fatalf("third StoreChat: %v", err)
	}

	var name string
	var last time.Time
	if err := db.QueryRow("SELECT name, last_message_time FROM chats WHERE jid = ?", jid).Scan(&name, &last); err != nil {
		t.Fatalf("reading chat: %v", err)
	}
	if name != "Kate Sydney Cook" {
		t.Errorf("name = %q, want it preserved", name)
	}
	if !last.Equal(second) {
		t.Errorf("last_message_time = %v, want it advanced to %v", last, second)
	}

	var messages int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid = ?", jid).Scan(&messages); err != nil {
		t.Fatalf("counting messages: %v", err)
	}
	if messages != 1 {
		t.Errorf("%d messages survived the chat upsert, want 1", messages)
	}
}

// The group exemption has to survive the write path, not just keepChatName:
// StoreChat is what GetChatName's group branch hands "2026" to, and it is the
// only place the name reaches the row.
func TestStoreChatKeepsDigitOnlyGroupName(t *testing.T) {
	db := newContactTestDB(t)
	store := &MessageStore{db: db}
	jid := "120363158681642690@g.us"
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	if err := store.StoreChat(jid, "2026", now); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	var name sql.NullString
	if err := db.QueryRow("SELECT name FROM chats WHERE jid = ?", jid).Scan(&name); err != nil {
		t.Fatalf("reading chat: %v", err)
	}
	if name.String != "2026" {
		t.Errorf("chats.name = (valid=%v, %q), want %q", name.Valid, name.String, "2026")
	}
}
