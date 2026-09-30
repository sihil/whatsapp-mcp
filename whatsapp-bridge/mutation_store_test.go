package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func reactionEmoji(t *testing.T, s *MessageStore, id, chat, sender string) string {
	t.Helper()
	var emoji string
	if err := s.db.QueryRow(
		"SELECT emoji FROM reactions WHERE message_id = ? AND chat_jid = ? AND sender = ?", id, chat, sender,
	).Scan(&emoji); err != nil {
		t.Fatalf("reaction by %s on %s: %v", sender, id, err)
	}
	return emoji
}

// History sync carries reactions on the message and deleted messages as
// REVOKE stubs; both must reach the store.
func TestApplyHistoryMutations(t *testing.T) {
	s := newMutationStore(t)
	ctx := context.Background()
	chat, _ := types.ParseJID(tChatGrp)
	own, _ := types.ParseJID(tMe)
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{"200000000000001@lid": pnJID("15550001002")}}
	apply := func(wmi *waWeb.WebMessageInfo) bool {
		return applyHistoryMutations(ctx, lids, s, wmi, chat, own, nil, waLog.Noop)
	}

	putMsg(t, s, "H1", tChatGrp, tPeer, "hello", "", false)
	reacted := &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String("H1"), Participant: proto.String(tPeer)},
		MessageTimestamp: proto.Uint64(uint64(t0.Unix())),
		Reactions: []*waWeb.Reaction{
			{Key: &waCommon.MessageKey{FromMe: proto.Bool(true)}, Text: proto.String("👍")},
			{Key: &waCommon.MessageKey{Participant: proto.String("200000000000001@lid")}, Text: proto.String("🎉")},
		},
	}
	if apply(reacted) {
		t.Error("a message with reactions must still be stored as a message")
	}
	if got := reactionEmoji(t, s, "H1", tChatGrp, tMe); got != "👍" {
		t.Errorf("own reaction = %q", got)
	}
	if got := reactionEmoji(t, s, "H1", tChatGrp, tOther); got != "🎉" {
		t.Errorf("reactor was not canonicalized to its PN JID: %q", got)
	}

	stub := &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String("H1"), Participant: proto.String(tPeer)},
		MessageTimestamp: proto.Uint64(uint64(t0.Unix())),
		MessageStubType:  waWeb.WebMessageInfo_REVOKE.Enum(),
	}
	if !apply(stub) {
		t.Error("a REVOKE stub must not be stored as a message")
	}
	if c, _, deleted, _, _ := msgState(t, s, "H1", tChatGrp); c != "" || !deleted {
		t.Errorf("stub did not tombstone the row: %q %v", c, deleted)
	}
	if n := countReactions(t, s, "H1", tChatGrp); n != 0 {
		t.Errorf("reactions left on a deleted message: %d", n)
	}

	stub.Key.ID = proto.String("UNKNOWN")
	if !apply(stub) {
		t.Error("a REVOKE stub for an unknown message is still a stub")
	}
}

// A message sent through /api/send has no echo, so it is stored at send time;
// otherwise it could not be edited or deleted.
func TestRecordSentMessage(t *testing.T) {
	s := newMutationStore(t)
	ctx := context.Background()
	own, _ := types.ParseJID("15550009999:7@s.whatsapp.net")
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{"200000000000001@lid": pnJID("15550001001")}}
	now := time.Now()

	text := &waE2E.Message{Conversation: proto.String("typo")}
	if err := recordSentMessage(ctx, lids, s, lidJID("200000000000001"), own, "S1", now, text); err != nil {
		t.Fatalf("recordSentMessage: %v", err)
	}
	m, err := s.GetStoredMessage(ctx, "S1", tChatDM)
	if err != nil {
		t.Fatalf("sent message not stored under the canonical chat key: %v", err)
	}
	if !m.IsFromMe || m.Sender != tMe || m.Content != "typo" {
		t.Errorf("stored row: %+v", m)
	}
	if why := checkEditable(m, "fixed", now); why != "" {
		t.Errorf("own send not editable: %s", why)
	}
	// The REST handlers key the store change on the row's own sender.
	if applied, err := s.ApplyEdit(tChatDM, "S1", m.Sender, "fixed", now); err != nil || !applied {
		t.Errorf("edit of own send not applied: %v %v", applied, err)
	}
	if applied, err := s.ApplyRevoke(tChatDM, "S1", m.Sender, nil, now); err != nil || !applied {
		t.Errorf("delete of own send not applied: %v %v", applied, err)
	}

	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: proto.String("http://u"), FileLength: proto.Uint64(3)}}
	if err := recordSentMessage(ctx, lids, s, pnJID("15550001001"), own, "S2", now, img); err != nil {
		t.Fatalf("recordSentMessage media: %v", err)
	}
	if _, _, _, url, mediaType := msgState(t, s, "S2", tChatDM); mediaType != "image" || url.String != "http://u" {
		t.Errorf("media row: %q %v", mediaType, url)
	}
}

// A legacy own row may carry a sender other than the current own JID; the
// handlers pass the stored sender, so the store still follows the send.
func TestOwnMutationAppliesToLegacySender(t *testing.T) {
	s := newMutationStore(t)
	legacy := "200000000000009@lid"
	putMsg(t, s, "L1", tChatDM, legacy, "old", "", true)
	m, err := s.GetStoredMessage(context.Background(), "L1", tChatDM)
	if err != nil {
		t.Fatal(err)
	}
	if applied, _ := s.ApplyEdit(tChatDM, "L1", tMe, "new", t0); applied {
		t.Fatal("test premise: the own JID does not match the legacy sender")
	}
	if applied, err := s.ApplyEdit(tChatDM, "L1", m.Sender, "new", t0); err != nil || !applied {
		t.Errorf("edit keyed on stored sender not applied: %v %v", applied, err)
	}
	if applied, err := s.ApplyRevoke(tChatDM, "L1", m.Sender, nil, t0); err != nil || !applied {
		t.Errorf("revoke keyed on stored sender not applied: %v %v", applied, err)
	}
}

// download_media on a tombstone must be refused from the Deleted flag: the
// media pointers are NULL and GetMediaInfo cannot scan them.
func TestDeletedMediaIsReportedDeleted(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "M1", tChatDM, tPeer, "", "image", false)
	if _, err := s.ApplyRevoke(tChatDM, "M1", tPeer, nil, t0); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetStoredMessage(context.Background(), "M1", tChatDM)
	if err != nil || !m.Deleted || m.MediaType != "image" {
		t.Errorf("tombstone: %+v %v", m, err)
	}
}

// The startup canonicalization renames chat keys and senders in messages;
// reactions must follow or they detach from their messages.
func TestCanonicalizeMovesReactions(t *testing.T) {
	s := newMutationStore(t)
	lidChat, lidPeer := "200000000000001@lid", "200000000000001@lid"
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{lidChat: pnJID("15550001001")}}
	putMsg(t, s, "R1", lidChat, lidPeer, "hi", "", false)
	putMsg(t, s, "R2", tChatGrp, tOther, "hi", "", false)
	for _, r := range [][4]string{
		{"R1", lidChat, lidPeer, "👍"},
		{"R1", lidChat, tMe, "❤️"},
		{"R2", tChatGrp, lidPeer, "old"}, // same reactor under both namespaces
		{"R2", tChatGrp, tPeer, "new"},
	} {
		if err := s.SetReaction(r[0], r[1], r[2], r[3], t0); err != nil {
			t.Fatal(err)
		}
	}

	for run := 1; run <= 2; run++ {
		if _, err := canonicalizeMessageStore(context.Background(), s.db, lids, pnJID("15550009999")); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if got := reactionEmoji(t, s, "R1", tChatDM, tPeer); got != "👍" {
			t.Errorf("run %d: peer reaction = %q", run, got)
		}
		if got := reactionEmoji(t, s, "R1", tChatDM, tMe); got != "❤️" {
			t.Errorf("run %d: own reaction = %q", run, got)
		}
		if n := countReactions(t, s, "R2", tChatGrp); n != 1 {
			t.Errorf("run %d: duplicate reactor rows: %d", run, n)
		}
		var stale int
		s.db.QueryRow("SELECT COUNT(*) FROM reactions WHERE chat_jid LIKE '%@lid' OR sender LIKE '%@lid'").Scan(&stale)
		if stale != 0 {
			t.Errorf("run %d: %d reactions still under @lid", run, stale)
		}
	}
}
