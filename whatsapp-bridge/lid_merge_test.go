package main

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// Once a mapping is learned, everything stored under the @lid moves to the
// phone number at once: one person no longer reads as two senders, and an
// edit or revoke naming them by phone number matches their earlier rows.
func TestMergeLIDsFoldsIdentity(t *testing.T) {
	s := newMutationStore(t)
	ctx := context.Background()
	lid := lidJID("200000000000001")
	putMsg(t, s, "G1", tChatGrp, lid.String(), "orig", "", false)
	putMsg(t, s, "G2", tChatGrp, tMe, "mine", "", true)
	putMsg(t, s, "D1", lid.String(), lid.String(), "dm", "", false)
	must(t, s.SetReaction("G2", tChatGrp, lid.String(), "👍", t0))

	if applied, _ := s.ApplyEdit(tChatGrp, "G1", tOther, "fixed", t0); applied {
		t.Fatal("test premise: the PN author does not match the @lid row")
	}
	must(t, s.MergeLIDs(ctx, map[string]types.JID{lid.String(): pnJID("15550001002")}))

	if applied, err := s.ApplyEdit(tChatGrp, "G1", tOther, "fixed", t0); err != nil || !applied {
		t.Errorf("edit by the PN-named author not applied after the merge: %v %v", applied, err)
	}
	if got := reactionEmoji(t, s, "G2", tChatGrp, tOther); got != "👍" {
		t.Errorf("reaction did not follow the merge: %q", got)
	}
	if _, err := s.GetStoredMessage(ctx, "D1", tOther); err != nil {
		t.Errorf("DM chat keyed by the @lid was not moved to the phone number: %v", err)
	}
	var alias string
	if err := s.db.QueryRow("SELECT new_jid FROM jid_aliases WHERE old_jid = ?", lid.String()).Scan(&alias); err != nil || alias != tOther {
		t.Errorf("no alias left for readers holding the @lid chat key: %q %v", alias, err)
	}
	var left int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE sender LIKE '%@lid' OR chat_jid LIKE '%@lid'").Scan(&left)
	if left != 0 {
		t.Errorf("%d rows still under the @lid", left)
	}
}

func TestMessageLIDs(t *testing.T) {
	lid, pn := lidJID("200000000000001"), pnJID("15550001002")
	grp, _ := types.ParseJID(tChatGrp)
	cases := []struct {
		name   string
		info   types.MessageSource
		chat   types.JID
		sender string
		want   bool
	}{
		{"LID-addressed with PN alt", types.MessageSource{Chat: grp, Sender: lid, SenderAlt: pn}, grp, pn.String(), true},
		{"PN-addressed with LID alt", types.MessageSource{Chat: grp, Sender: pn, SenderAlt: lid}, grp, pn.String(), true},
		{"DM chat keyed by the LID", types.MessageSource{Chat: lid, Sender: lid}, pn, pn.String(), true},
		{"mapping unknown", types.MessageSource{Chat: grp, Sender: lid}, grp, lid.String(), false},
	}
	for _, c := range cases {
		got := messageLIDs(c.info, c.chat, c.sender)
		if ok := got[lid.String()] == pn; ok != c.want || len(got) > 1 {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
