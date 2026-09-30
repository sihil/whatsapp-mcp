package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const (
	tChatDM  = "15550001001@s.whatsapp.net"
	tChatGrp = "120363158681642690@g.us"
	tMe      = "15550009999@s.whatsapp.net"
	tPeer    = "15550001001@s.whatsapp.net"
	tOther   = "15550001002@s.whatsapp.net"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newMutationStore(t *testing.T) *MessageStore {
	t.Helper()
	t.Chdir(t.TempDir())
	store, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func putMsg(t *testing.T, s *MessageStore, id, chat, sender, content, mediaType string, fromMe bool) {
	t.Helper()
	if err := s.StoreChat(chat, "", t0); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	if err := s.StoreMessage(id, chat, sender, content, t0, fromMe, mediaType, "", "http://u", []byte{1}, []byte{2}, []byte{3}, 9); err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}
}

func msgState(t *testing.T, s *MessageStore, id, chat string) (content string, edited, deleted bool, url sql.NullString, mediaType string) {
	t.Helper()
	var e, d sql.NullTime
	var mt sql.NullString
	if err := s.db.QueryRow(
		"SELECT content, edited_at, deleted_at, url, media_type FROM messages WHERE id = ? AND chat_jid = ?", id, chat,
	).Scan(&content, &e, &d, &url, &mt); err != nil {
		t.Fatalf("reading message: %v", err)
	}
	return content, e.Valid, d.Valid, url, mt.String
}

func countReactions(t *testing.T, s *MessageStore, id, chat string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM reactions WHERE message_id = ? AND chat_jid = ?", id, chat).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateMutationsIdempotentOnOldSchema(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (id TEXT, chat_jid TEXT, content TEXT, PRIMARY KEY (id, chat_jid));
		INSERT INTO messages VALUES ('A', 'c', 'keep me')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateMutations(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var content string
	var e, d sql.NullTime
	if err := db.QueryRow("SELECT content, edited_at, deleted_at FROM messages").Scan(&content, &e, &d); err != nil {
		t.Fatalf("new columns missing: %v", err)
	}
	if content != "keep me" || e.Valid || d.Valid {
		t.Errorf("row changed by migration: %q %v %v", content, e, d)
	}
	if _, err := db.Exec("INSERT INTO reactions (message_id, chat_jid, sender, emoji) VALUES ('A','c','s','x')"); err != nil {
		t.Errorf("reactions table unusable: %v", err)
	}
}

func TestNewMessageStoreIsIdempotent(t *testing.T) {
	store := newMutationStore(t)
	if err := migrateMutations(store.db); err != nil {
		t.Fatalf("re-running migration on a fresh store: %v", err)
	}
}

func TestEditAuthorization(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "M1", tChatDM, tPeer, "orig", "", false)

	cases := []struct {
		name, editor, text string
		want               bool
	}{
		{"other sender", tOther, "hacked", false},
		{"empty text", tPeer, "", false},
		{"author", tPeer, "fixed", true},
	}
	for _, c := range cases {
		got, err := s.ApplyEdit(tChatDM, "M1", c.editor, c.text, t0.Add(time.Minute))
		if err != nil || got != c.want {
			t.Errorf("%s: applied=%v err=%v, want %v", c.name, got, err, c.want)
		}
	}
	content, edited, _, _, _ := msgState(t, s, "M1", tChatDM)
	if content != "fixed" || !edited {
		t.Errorf("content=%q edited=%v", content, edited)
	}
	if ok, err := s.ApplyEdit(tChatDM, "MISSING", tPeer, "x", t0); ok || err != nil {
		t.Errorf("unknown target: applied=%v err=%v", ok, err)
	}
}

func TestRevokeTombstone(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "M1", tChatDM, tPeer, "secret", "image", false)
	if err := s.SetReaction("M1", tChatDM, tMe, "👍", t0); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ApplyRevoke(tChatDM, "M1", tOther, nil, t0); ok {
		t.Error("DM revoke by a third party was applied")
	}
	if ok, err := s.ApplyRevoke(tChatDM, "M1", tPeer, nil, t0); !ok || err != nil {
		t.Fatalf("author revoke: %v %v", ok, err)
	}
	content, _, deleted, url, mt := msgState(t, s, "M1", tChatDM)
	if content != "" || !deleted || url.Valid || mt != "image" {
		t.Errorf("tombstone wrong: content=%q deleted=%v url=%v media=%q", content, deleted, url, mt)
	}
	var key, sha, length sql.NullString
	s.db.QueryRow("SELECT media_key, file_sha256, file_length FROM messages WHERE id='M1'").Scan(&key, &sha, &length)
	if key.Valid || sha.Valid || length.Valid {
		t.Error("media pointers not cleared")
	}
	if n := countReactions(t, s, "M1", tChatDM); n != 0 {
		t.Errorf("%d reactions survived the delete", n)
	}
	if ok, _ := s.ApplyRevoke(tChatDM, "MISSING", tPeer, nil, t0); ok {
		t.Error("unknown target applied")
	}
	// Edits and reactions no longer land on the tombstone.
	if ok, _ := s.ApplyEdit(tChatDM, "M1", tPeer, "back", t0); ok {
		t.Error("edited a tombstone")
	}
	s.SetReaction("M1", tChatDM, tOther, "🔥", t0)
	if n := countReactions(t, s, "M1", tChatDM); n != 0 {
		t.Error("reaction stored on a tombstone")
	}
}

func TestReactionUpsertAndRemove(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "M1", tChatDM, tPeer, "hi", "", false)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetReaction("M1", tChatDM, tMe, "👍", t0))
	must(s.SetReaction("M1", tChatDM, tMe, "❤️", t0.Add(time.Second)))
	must(s.SetReaction("M1", tChatDM, tPeer, "😂", t0))
	var emoji string
	s.db.QueryRow("SELECT emoji FROM reactions WHERE sender = ?", tMe).Scan(&emoji)
	if emoji != "❤️" || countReactions(t, s, "M1", tChatDM) != 2 {
		t.Errorf("emoji=%q count=%d", emoji, countReactions(t, s, "M1", tChatDM))
	}
	must(s.SetReaction("M1", tChatDM, tMe, "", t0))
	if n := countReactions(t, s, "M1", tChatDM); n != 1 {
		t.Errorf("count after remove = %d, want 1", n)
	}
	var blank int
	s.db.QueryRow("SELECT COUNT(*) FROM reactions WHERE emoji = ''").Scan(&blank)
	if blank != 0 {
		t.Error("stored an empty emoji")
	}
}

func TestStoreMessageKeepsTombstoneAndEdit(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "E1", tChatDM, tPeer, "orig", "", false)
	s.ApplyEdit(tChatDM, "E1", tPeer, "edited", t0.Add(time.Minute))
	putMsg(t, s, "E1", tChatDM, tPeer, "orig", "", false) // re-delivered
	if c, edited, _, _, _ := msgState(t, s, "E1", tChatDM); c != "edited" || !edited {
		t.Errorf("edit lost: %q %v", c, edited)
	}

	putMsg(t, s, "D1", tChatDM, tPeer, "orig", "image", false)
	s.ApplyRevoke(tChatDM, "D1", tPeer, nil, t0)
	putMsg(t, s, "D1", tChatDM, tPeer, "orig", "image", false)
	if c, _, deleted, url, _ := msgState(t, s, "D1", tChatDM); c != "" || !deleted || url.Valid {
		t.Errorf("tombstone resurrected: %q %v %v", c, deleted, url)
	}
}

func TestEditWindow(t *testing.T) {
	now := t0.Add(time.Hour)
	if !withinEditWindow(now.Add(-14*time.Minute), now) || !withinEditWindow(now.Add(-15*time.Minute), now) {
		t.Error("inside the window rejected")
	}
	if withinEditWindow(now.Add(-16*time.Minute), now) {
		t.Error("outside the window accepted")
	}
}

func TestCheckEditableAndDeletable(t *testing.T) {
	now := t0.Add(time.Minute)
	ok := StoredMessage{IsFromMe: true, Time: t0}
	cases := []struct {
		name string
		m    StoredMessage
		text string
		ok   bool
	}{
		{"fine", ok, "x", true},
		{"not mine", StoredMessage{Time: t0}, "x", false},
		{"deleted", StoredMessage{IsFromMe: true, Time: t0, Deleted: true}, "x", false},
		{"media", StoredMessage{IsFromMe: true, Time: t0, MediaType: "image"}, "x", false},
		{"empty", ok, "", false},
		{"old", StoredMessage{IsFromMe: true, Time: t0.Add(-time.Hour)}, "x", false},
	}
	for _, c := range cases {
		if got := checkEditable(c.m, c.text, now) == ""; got != c.ok {
			t.Errorf("checkEditable %s = %v, want %v", c.name, got, c.ok)
		}
	}
	del := func(m StoredMessage, now time.Time) bool { return checkDeletable(m, now) == "" }
	if !del(ok, now) || del(StoredMessage{Time: t0}, now) ||
		del(StoredMessage{IsFromMe: true, Time: t0, Deleted: true}, now) {
		t.Error("checkDeletable wrong")
	}
	if !del(ok, t0.Add(deleteWindow)) || del(ok, t0.Add(deleteWindow+time.Hour)) {
		t.Error("checkDeletable ignores the 60-hour delete-for-everyone window")
	}
}

func TestClassifyMutation(t *testing.T) {
	edit := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: &waE2E.Message{Conversation: proto.String("new")},
		TimestampMS:   proto.Int64(1000),
	}}
	wrapped := &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: edit}}
	for name, m := range map[string]*waE2E.Message{"unwrapped": edit, "wrapped": wrapped} {
		mu := classifyMutation(m)
		if mu.kind != mutationEdit || mu.text != "new" || mu.at.UnixMilli() != 1000 {
			t.Errorf("%s edit: %+v", name, mu)
		}
	}
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}}
	if classifyMutation(revoke).kind != mutationRevoke {
		t.Error("revoke not classified")
	}
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("")}}
	if mu := classifyMutation(react); mu.kind != mutationReaction || mu.emoji != "" {
		t.Errorf("reaction: %+v", mu)
	}
	if classifyMutation(&waE2E.Message{Conversation: proto.String("hi")}).kind != mutationNone {
		t.Error("plain text classified as a mutation")
	}
}
