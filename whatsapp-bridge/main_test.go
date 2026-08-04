package main

import (
	"context"
	"database/sql"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

type fakeLIDMapper struct {
	pnForLID map[string]types.JID
}

func (f fakeLIDMapper) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f.pnForLID[lid.String()], nil
}

func TestResolveReplyParticipant(t *testing.T) {
	groupJID := types.JID{User: "120363000000000000", Server: types.GroupServer}
	dmJID := types.JID{User: "16465559999", Server: types.DefaultUserServer}
	ownJID := types.JID{User: "15550001111", Server: types.DefaultUserServer}
	lids := fakeLIDMapper{
		pnForLID: map[string]types.JID{
			"113108114763976@lid": {User: "16175551234", Server: types.DefaultUserServer},
		},
	}

	tests := []struct {
		name         string
		storedSender string
		isFromMe     bool
		chatJID      types.JID
		want         types.JID
	}{
		{
			name:         "lid sender is parsed as-is",
			storedSender: "74616466886757@lid",
			chatJID:      groupJID,
			want:         types.JID{User: "74616466886757", Server: types.HiddenUserServer},
		},
		{
			name:         "s.whatsapp.net sender is parsed as-is",
			storedSender: "16465551234@s.whatsapp.net",
			chatJID:      groupJID,
			want:         types.JID{User: "16465551234", Server: types.DefaultUserServer},
		},
		{
			name:         "bare-digit sender in a DM resolves to the chat JID",
			storedSender: "16465559999",
			chatJID:      dmJID,
			want:         dmJID,
		},
		{
			name:         "bare-digit group sender known to the LID store becomes an @lid JID",
			storedSender: "113108114763976",
			chatJID:      groupJID,
			want:         types.JID{User: "113108114763976", Server: types.HiddenUserServer},
		},
		{
			name:         "bare-digit group sender without mapping falls back to default user server",
			storedSender: "16465551234",
			chatJID:      groupJID,
			want:         types.JID{User: "16465551234", Server: types.DefaultUserServer},
		},
		{
			name:         "is_from_me row resolves to the bridge's own JID",
			storedSender: "15550001111",
			isFromMe:     true,
			chatJID:      groupJID,
			want:         ownJID,
		},
		{
			name:         "is_from_me row with a full-JID sender still resolves to own JID",
			storedSender: "16465551234@s.whatsapp.net",
			isFromMe:     true,
			chatJID:      groupJID,
			want:         ownJID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReplyParticipant(context.Background(), lids, tt.storedSender, tt.isFromMe, tt.chatJID, ownJID)
			if got != tt.want {
				t.Errorf("resolveReplyParticipant(%q, %v, %v, %v) = %v, want %v",
					tt.storedSender, tt.isFromMe, tt.chatJID, ownJID, got, tt.want)
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
		wantQuotedText string
	}{
		{"non-empty content is quoted verbatim", "ABCD1234", "hello there", "hello there"},
		{"empty content falls back to a placeholder", "EFGH5678", "", "[media]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildReplyContextInfo(tt.id, participant, tt.content)

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
		id TEXT, chat_jid TEXT, sender TEXT, content TEXT, is_from_me BOOLEAN,
		PRIMARY KEY (id, chat_jid)
	)`); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	store := &MessageStore{db: db}

	const msgID = "MSG1"
	chatA, chatB := "16465559999@s.whatsapp.net", "120363000000000000@g.us"
	for _, row := range []struct {
		chat, sender, content string
		isFromMe              bool
	}{
		{chatA, "16465559999", "hello from A", false},
		{chatB, "74616466886757@lid", "hello from B", true},
	} {
		if _, err := db.Exec("INSERT INTO messages (id, chat_jid, sender, content, is_from_me) VALUES (?, ?, ?, ?, ?)",
			msgID, row.chat, row.sender, row.content, row.isFromMe); err != nil {
			t.Fatalf("inserting row: %v", err)
		}
	}

	sender, content, isFromMe, err := store.GetMessageForReply(msgID, chatA)
	if err != nil || sender != "16465559999" || content != "hello from A" || isFromMe {
		t.Errorf("chat A lookup = (%q, %q, %v, %v), want (16465559999, hello from A, false, nil)", sender, content, isFromMe, err)
	}

	sender, content, isFromMe, err = store.GetMessageForReply(msgID, chatB)
	if err != nil || sender != "74616466886757@lid" || content != "hello from B" || !isFromMe {
		t.Errorf("chat B lookup = (%q, %q, %v, %v), want (74616466886757@lid, hello from B, true, nil)", sender, content, isFromMe, err)
	}

	if _, _, _, err := store.GetMessageForReply(msgID, "19998887777@s.whatsapp.net"); err != sql.ErrNoRows {
		t.Errorf("lookup in uninvolved chat: err = %v, want sql.ErrNoRows", err)
	}
	if _, _, _, err := store.GetMessageForReply("", chatA); err != sql.ErrNoRows {
		t.Errorf("empty id lookup: err = %v, want sql.ErrNoRows", err)
	}
}
