package main

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func participantsOf(t *testing.T, s *MessageStore, group string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT participant FROM group_participants WHERE group_jid = ? ORDER BY participant`, group)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func withParticipants(info *types.GroupInfo, ps ...types.GroupParticipant) *types.GroupInfo {
	info.Participants = ps
	return info
}

var participantLIDs = fakeLIDMapper{pnForLID: map[string]types.JID{
	"200000000000002@lid": pnJID("15550001002"),
}}

func TestSyncRecordsJoinedGroupParticipants(t *testing.T) {
	s := newMutationStore(t)
	lister := &fakeGroupLister{joined: []*types.GroupInfo{
		withParticipants(groupInfo(tStandard, "Book club", "", false, false),
			// Phone number known from the participant list itself.
			types.GroupParticipant{JID: lidJID("200000000000001"), PhoneNumber: pnJID("15550001001")},
			// LID only, mapped through the LID store.
			types.GroupParticipant{JID: lidJID("200000000000002")},
			// LID only and unmapped: kept as the LID.
			types.GroupParticipant{JID: lidJID("200000000000003")},
		),
	}}
	if _, _, err := syncGroupLinks(context.Background(), lister, participantLIDs, s, t0, func() {}); err != nil {
		t.Fatal(err)
	}
	want := []string{"15550001001@s.whatsapp.net", "15550001002@s.whatsapp.net", "200000000000003@lid"}
	if got := participantsOf(t, s, tStandard); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("participants = %v, want %v", got, want)
	}

	// A later sync replaces the list, so someone who left is gone.
	lister.joined = []*types.GroupInfo{withParticipants(groupInfo(tStandard, "Book club", "", false, false),
		types.GroupParticipant{JID: pnJID("15550001001")})}
	if _, _, err := syncGroupLinks(context.Background(), lister, participantLIDs, s, t0, func() {}); err != nil {
		t.Fatal(err)
	}
	if got := participantsOf(t, s, tStandard); fmt.Sprint(got) != "[15550001001@s.whatsapp.net]" {
		t.Errorf("after resync = %v", got)
	}
}

func TestSyncDropsParticipantsOfGroupsLeft(t *testing.T) {
	s := newMutationStore(t)
	lister := &fakeGroupLister{joined: []*types.GroupInfo{
		withParticipants(groupInfo(tStandard, "Book club", "", false, false), types.GroupParticipant{JID: pnJID("15550001001")}),
	}}
	ctx := context.Background()
	must(t, func() error { _, _, err := syncGroupLinks(ctx, lister, participantLIDs, s, t0, func() {}); return err }())
	lister.joined = nil
	must(t, func() error { _, _, err := syncGroupLinks(ctx, lister, participantLIDs, s, t0, func() {}); return err }())
	if got := participantsOf(t, s, tStandard); len(got) != 0 {
		t.Errorf("participants of a group left remain: %v", got)
	}
}

func TestSyncRecordsCommunityWideMembers(t *testing.T) {
	s := newMutationStore(t)
	lister := &fakeGroupLister{
		joined: []*types.GroupInfo{groupInfo(tSubgroup, "Corstorphine", tCommunity, false, false)},
		subGroups: map[string][]*types.GroupLinkTarget{
			tCommunity: {linkTarget(tSubgroup, "Corstorphine", false), linkTarget(tHidden, "Leith", false)},
		},
		communityMembers: map[string][]types.JID{
			tCommunity: {pnJID("15550001001"), lidJID("200000000000002")},
		},
	}
	if _, _, err := syncGroupLinks(context.Background(), lister, participantLIDs, s, t0, func() {}); err != nil {
		t.Fatal(err)
	}
	want := "[15550001001@s.whatsapp.net 15550001002@s.whatsapp.net]"
	if got := participantsOf(t, s, tCommunity); fmt.Sprint(got) != want {
		t.Errorf("community members = %v, want %v", got, want)
	}
}

func TestCommunityMembersRateLimitEndsThePass(t *testing.T) {
	s := newMutationStore(t)
	lister := &fakeGroupLister{
		joined:         []*types.GroupInfo{groupInfo(tCommunity, "Edinburgh", "", true, false)},
		subGroups:      map[string][]*types.GroupLinkTarget{tCommunity: {}},
		memberFailures: map[string]error{tCommunity: whatsmeow.ErrIQRateOverLimit},
	}
	_, remaining, err := syncGroupLinks(context.Background(), lister, participantLIDs, s, t0, func() {})
	if err != errSubGroupsRateLimited || remaining != 1 {
		t.Errorf("err %v remaining %d, want a rate limit with the community still due", err, remaining)
	}
}

func TestLiveJoinsAndLeavesUpdateParticipants(t *testing.T) {
	s := newMutationStore(t)
	sender := pnJID("15550001003")
	info := &events.GroupInfo{JID: parsed(tStandard), Sender: &sender,
		Join: []types.JID{pnJID("15550001001"), lidJID("200000000000002")}}
	must(t, s.ApplyParticipantChanges(groupEventsFromInfo(context.Background(), participantLIDs, info)))
	if got := participantsOf(t, s, tStandard); fmt.Sprint(got) != "[15550001001@s.whatsapp.net 15550001002@s.whatsapp.net]" {
		t.Errorf("after joins = %v", got)
	}
	info = &events.GroupInfo{JID: parsed(tStandard), Sender: &sender, Leave: []types.JID{pnJID("15550001001")}}
	must(t, s.ApplyParticipantChanges(groupEventsFromInfo(context.Background(), participantLIDs, info)))
	if got := participantsOf(t, s, tStandard); fmt.Sprint(got) != "[15550001002@s.whatsapp.net]" {
		t.Errorf("after leave = %v", got)
	}
}

func TestParticipantIdentityMovesToPhoneNumber(t *testing.T) {
	s := newMutationStore(t)
	lid, pn := "200000000000009@lid", "15550001009@s.whatsapp.net"
	for _, row := range [][2]string{{tStandard, lid}, {tSubgroup, lid}, {tSubgroup, pn}} {
		_, err := s.db.Exec(`INSERT INTO group_participants (group_jid, participant) VALUES (?, ?)`, row[0], row[1])
		must(t, err)
	}
	tx, err := s.db.Begin()
	must(t, err)
	must(t, moveParticipantIdentity(context.Background(), tx, lid, pn))
	must(t, tx.Commit())

	rows, err := s.db.Query(`SELECT group_jid || ' ' || participant FROM group_participants`)
	must(t, err)
	var got []string
	for rows.Next() {
		var r string
		must(t, rows.Scan(&r))
		got = append(got, r)
	}
	rows.Close()
	sort.Strings(got)
	want := []string{tSubgroup + " " + pn, tStandard + " " + pn}
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

func TestCanonicalizeRewritesParticipants(t *testing.T) {
	s := newMutationStore(t)
	_, err := s.db.Exec(`INSERT INTO group_participants (group_jid, participant) VALUES (?, '200000000000002@lid')`, tStandard)
	must(t, err)
	if _, err := canonicalizeMessageStore(context.Background(), s.db, participantLIDs, pnJID("15550009999")); err != nil {
		t.Fatal(err)
	}
	if got := participantsOf(t, s, tStandard); fmt.Sprint(got) != "[15550001002@s.whatsapp.net]" {
		t.Errorf("participants = %v", got)
	}
}
