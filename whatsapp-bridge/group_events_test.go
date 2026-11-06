package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const tLIDOther = "200000000000002@lid"

var groupLIDs = fakeLIDMapper{pnForLID: map[string]types.JID{tLIDOther: pnJID("15550001002")}}

func jidPtr(s string) *types.JID {
	jid, _ := types.ParseJID(s)
	return &jid
}

func parsed(s string) types.JID {
	jid, _ := types.ParseJID(s)
	return jid
}

func TestGroupEventsFromInfo(t *testing.T) {
	chat := parsed(tChatGrp)
	at := t0.Add(1500 * time.Millisecond)
	cases := []struct {
		name string
		info events.GroupInfo
		want []groupEvent
	}{
		{
			name: "joined by invite link, LID participant",
			info: events.GroupInfo{JID: chat, Timestamp: at, JoinReason: "invite", Join: []types.JID{parsed(tLIDOther)}},
			want: []groupEvent{{tChatGrp, "join", tOther, "", "invite", t0.Add(time.Second)}},
		},
		{
			name: "added by an admin named by LID with a PN alt",
			info: events.GroupInfo{JID: chat, Timestamp: t0, Sender: jidPtr("200000000000009@lid"), SenderPN: jidPtr(tPeer),
				Join: []types.JID{parsed(tOther)}},
			want: []groupEvent{{tChatGrp, "join", tOther, tPeer, "added", t0}},
		},
		{
			name: "left of their own accord",
			info: events.GroupInfo{JID: chat, Timestamp: t0, Sender: jidPtr(tOther), Leave: []types.JID{parsed(tOther)}},
			want: []groupEvent{{tChatGrp, "leave", tOther, tOther, "left", t0}},
		},
		{
			name: "removed by an admin",
			info: events.GroupInfo{JID: chat, Timestamp: t0, Sender: jidPtr(tPeer), Leave: []types.JID{parsed(tOther)}},
			want: []groupEvent{{tChatGrp, "leave", tOther, tPeer, "removed", t0}},
		},
		{
			name: "promoted and demoted together",
			info: events.GroupInfo{JID: chat, Timestamp: t0, Sender: jidPtr(tMe),
				Promote: []types.JID{parsed(tOther)}, Demote: []types.JID{parsed(tPeer)}},
			want: []groupEvent{
				{tChatGrp, "promote", tOther, tMe, "", t0},
				{tChatGrp, "demote", tPeer, tMe, "", t0},
			},
		},
		{
			name: "name change only",
			info: events.GroupInfo{JID: chat, Timestamp: t0, Name: &types.GroupName{Name: "x"}},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := groupEventsFromInfo(context.Background(), groupLIDs, &tc.info)
			assertGroupEvents(t, got, tc.want)
		})
	}
}

func historyStub(stub waWeb.WebMessageInfo_StubType, actor string, fromMe bool, params ...string) *waWeb.WebMessageInfo {
	return &waWeb.WebMessageInfo{
		Key:                   &waCommon.MessageKey{ID: proto.String("STUB"), FromMe: proto.Bool(fromMe), Participant: proto.String(actor)},
		MessageTimestamp:      proto.Uint64(uint64(t0.Unix())),
		MessageStubType:       stub.Enum(),
		MessageStubParameters: params,
	}
}

func TestGroupEventsFromHistory(t *testing.T) {
	chat, own := parsed(tChatGrp), parsed(tMe)
	pnMap := map[string]types.JID{"200000000000009@lid": pnJID("15550001001")}
	cases := []struct {
		name   string
		wmi    *waWeb.WebMessageInfo
		isStub bool
		want   []groupEvent
	}{
		{
			name:   "added by me",
			wmi:    historyStub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD, "", true, tOther),
			isStub: true,
			want:   []groupEvent{{tChatGrp, "join", tOther, tMe, "added", t0}},
		},
		{
			name:   "joined by invite link",
			wmi:    historyStub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE, tLIDOther, false, tLIDOther),
			isStub: true,
			want:   []groupEvent{{tChatGrp, "join", tOther, tOther, "invite", t0}},
		},
		{
			name:   "left, named through the payload's own LID table",
			wmi:    historyStub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE, "200000000000009@lid", false, "200000000000009@lid"),
			isStub: true,
			want:   []groupEvent{{tChatGrp, "leave", tPeer, tPeer, "left", t0}},
		},
		{
			name:   "removed",
			wmi:    historyStub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE, tPeer, false, tOther),
			isStub: true,
			want:   []groupEvent{{tChatGrp, "leave", tOther, tPeer, "removed", t0}},
		},
		{
			name:   "promoted",
			wmi:    historyStub(waWeb.WebMessageInfo_GROUP_PARTICIPANT_PROMOTE, tPeer, false, tOther),
			isStub: true,
			want:   []groupEvent{{tChatGrp, "promote", tOther, tPeer, "", t0}},
		},
		{
			name:   "a deleted message is not a membership change",
			wmi:    historyStub(waWeb.WebMessageInfo_REVOKE, tPeer, false),
			isStub: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, isStub := groupEventsFromHistory(context.Background(), groupLIDs, tc.wmi, chat, own, pnMap)
			if isStub != tc.isStub {
				t.Fatalf("isStub = %v, want %v", isStub, tc.isStub)
			}
			assertGroupEvents(t, got, tc.want)
		})
	}
}

func countGroupEvents(t *testing.T, s *MessageStore, where string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM group_events WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatalf("counting group events: %v", err)
	}
	return n
}

// The same change can arrive live and again in a history sync; it must be
// stored once.
func TestStoreGroupEventsDeduplicates(t *testing.T) {
	s := newMutationStore(t)
	live := groupEvent{tChatGrp, "join", tOther, tPeer, "added", t0.Add(400 * time.Millisecond)}
	history := groupEvent{tChatGrp, "join", tOther, "", "", t0}
	must(t, s.StoreGroupEvents([]groupEvent{live}))
	must(t, s.StoreGroupEvents([]groupEvent{live, history}))
	if n := countGroupEvents(t, s, "1=1"); n != 1 {
		t.Errorf("%d rows for one membership change", n)
	}
	if n := countGroupEvents(t, s, "actor = ? AND reason = ?", tPeer, "added"); n != 1 {
		t.Error("a later, less detailed copy overwrote the stored detail")
	}
}

func TestMergeLIDsMovesGroupEvents(t *testing.T) {
	s := newMutationStore(t)
	must(t, s.StoreGroupEvents([]groupEvent{
		{tChatGrp, "join", tLIDOther, tLIDOther, "invite", t0},
		{tChatGrp, "promote", tPeer, tLIDOther, "", t0},
	}))
	must(t, s.MergeLIDs(context.Background(), map[string]types.JID{tLIDOther: pnJID("15550001002")}))
	if n := countGroupEvents(t, s, "participant LIKE '%@lid' OR actor LIKE '%@lid'"); n != 0 {
		t.Errorf("%d group events still name the @lid", n)
	}
	if n := countGroupEvents(t, s, "participant = ? AND actor = ?", tOther, tOther); n != 1 {
		t.Error("join did not follow the merge")
	}
}

func TestCanonicalizeMovesGroupEvents(t *testing.T) {
	s := newMutationStore(t)
	must(t, s.StoreGroupEvents([]groupEvent{
		{tChatGrp, "join", tLIDOther, "", "", t0},
		{tChatGrp, "join", tOther, tPeer, "added", t0}, // the same change, already under the PN
		{tChatGrp, "leave", "15550001001@c.us", tLIDOther, "removed", t0},
	}))
	for run := 1; run <= 2; run++ {
		if _, err := canonicalizeMessageStore(context.Background(), s.db, groupLIDs, pnJID("15550009999")); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if n := countGroupEvents(t, s, "participant NOT LIKE '%@s.whatsapp.net' OR actor LIKE '%@lid' OR actor LIKE '%@c.us'"); n != 0 {
			t.Errorf("run %d: %d group events not canonical", run, n)
		}
		if n := countGroupEvents(t, s, "kind = 'join'"); n != 1 {
			t.Errorf("run %d: %d join rows after merging namespaces", run, n)
		}
		if n := countGroupEvents(t, s, "kind = 'leave' AND participant = ? AND actor = ?", tPeer, tOther); n != 1 {
			t.Errorf("run %d: leave row not canonicalised", run)
		}
	}
}

func assertGroupEvents(t *testing.T, got, want []groupEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.chat != w.chat || g.kind != w.kind || g.participant != w.participant ||
			g.actor != w.actor || g.reason != w.reason || !g.at.Equal(w.at) {
			t.Errorf("event %d = %+v, want %+v", i, g, w)
		}
	}
}
