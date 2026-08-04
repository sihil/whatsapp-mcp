package main

import (
	"context"
	"database/sql"
	"testing"

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
	store := &MessageStore{db: db}

	pnKey := types.JID{User: "16465559999", Server: types.DefaultUserServer}
	lidKey := types.JID{User: "74616466886757", Server: types.HiddenUserServer}
	if _, err := db.Exec("INSERT INTO messages (id, chat_jid) VALUES ('MSG1', ?)", pnKey.String()); err != nil {
		t.Fatalf("inserting row: %v", err)
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
