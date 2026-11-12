package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	tCommunity = "120363000000000100@g.us"
	tAnnounce  = "120363000000000101@g.us"
	tSubgroup  = "120363000000000102@g.us"
	tHidden    = "120363000000000103@g.us"
	tStandard  = "120363000000000104@g.us"
)

type linkRow struct {
	parent                      string
	isCommunity, isAnnouncement bool
	name                        string
}

func groupLink(t *testing.T, s *MessageStore, jid string) linkRow {
	t.Helper()
	var r linkRow
	if err := s.db.QueryRow(
		"SELECT parent_jid, is_community, is_announcement, name FROM group_links WHERE group_jid = ?", jid,
	).Scan(&r.parent, &r.isCommunity, &r.isAnnouncement, &r.name); err != nil {
		t.Fatalf("group_links row for %s: %v", jid, err)
	}
	return r
}

func groupInfo(jid, name, parent string, isCommunity, isAnnouncement bool) *types.GroupInfo {
	info := &types.GroupInfo{JID: parsed(jid)}
	info.Name = name
	info.IsParent = isCommunity
	info.IsDefaultSubGroup = isAnnouncement
	if parent != "" {
		info.LinkedParentJID = parsed(parent)
	}
	return info
}

func linkTarget(jid, name string, isAnnouncement bool) *types.GroupLinkTarget {
	target := &types.GroupLinkTarget{JID: parsed(jid)}
	target.Name = name
	target.IsDefaultSubGroup = isAnnouncement
	return target
}

func TestPutGroupInfoRecordsCommunityStructure(t *testing.T) {
	s := newMutationStore(t)
	for _, info := range []*types.GroupInfo{
		groupInfo(tCommunity, "Edinburgh", "", true, false),
		groupInfo(tAnnounce, "Edinburgh announcements", tCommunity, false, true),
		groupInfo(tSubgroup, "Corstorphine", tCommunity, false, false),
		groupInfo(tStandard, "Book club", "", false, false),
	} {
		must(t, s.PutGroupInfo(info))
	}
	cases := map[string]linkRow{
		tCommunity: {"", true, false, "Edinburgh"},
		tAnnounce:  {tCommunity, false, true, "Edinburgh announcements"},
		tSubgroup:  {tCommunity, false, false, "Corstorphine"},
		tStandard:  {"", false, false, "Book club"},
	}
	for jid, want := range cases {
		if got := groupLink(t, s, jid); got != want {
			t.Errorf("%s = %+v, want %+v", jid, got, want)
		}
	}

	// A later snapshot that knows no name must not blank the stored one.
	must(t, s.PutGroupInfo(groupInfo(tSubgroup, "", tCommunity, false, false)))
	if got := groupLink(t, s, tSubgroup).name; got != "Corstorphine" {
		t.Errorf("name blanked: %q", got)
	}
}

type fakeGroupLister struct {
	joined    []*types.GroupInfo
	subGroups map[string][]*types.GroupLinkTarget
	failures  map[string]error
	asked     []string
}

func (f *fakeGroupLister) GetJoinedGroups(context.Context) ([]*types.GroupInfo, error) {
	return f.joined, nil
}

func (f *fakeGroupLister) GetSubGroups(_ context.Context, community types.JID) ([]*types.GroupLinkTarget, error) {
	f.asked = append(f.asked, community.String())
	if err := f.failures[community.String()]; err != nil {
		return nil, err
	}
	subs, ok := f.subGroups[community.String()]
	if !ok {
		return nil, errors.New("not a member")
	}
	return subs, nil
}

// A community the account is in only through a subgroup still has its
// subgroups recorded, including the ones the account is not in.
func TestSyncGroupLinks(t *testing.T) {
	s := newMutationStore(t)
	lister := &fakeGroupLister{
		joined: []*types.GroupInfo{
			groupInfo(tSubgroup, "Corstorphine", tCommunity, false, false),
			groupInfo(tStandard, "Book club", "", false, false),
		},
		subGroups: map[string][]*types.GroupLinkTarget{
			tCommunity: {
				linkTarget(tAnnounce, "Edinburgh announcements", true),
				linkTarget(tSubgroup, "Corstorphine", false),
				linkTarget(tHidden, "Leith", false),
			},
		},
	}
	recorded, remaining, err := syncGroupLinks(context.Background(), lister, s, t0, func() {})
	if err != nil {
		t.Fatalf("syncGroupLinks: %v", err)
	}
	if recorded != 1 || remaining != 0 || len(lister.asked) != 1 || lister.asked[0] != tCommunity {
		t.Errorf("recorded %d, remaining %d, asked %v", recorded, remaining, lister.asked)
	}
	if !groupLink(t, s, tCommunity).isCommunity {
		t.Error("community not marked")
	}
	if got := groupLink(t, s, tHidden); got.parent != tCommunity || got.name != "Leith" {
		t.Errorf("subgroup the account is not in = %+v", got)
	}
	if !groupLink(t, s, tAnnounce).isAnnouncement {
		t.Error("announcement group not marked")
	}
	if got := groupLink(t, s, tStandard).parent; got != "" {
		t.Errorf("standalone group given a parent %q", got)
	}
}

func TestApplyGroupLinkChanges(t *testing.T) {
	s := newMutationStore(t)
	ctx := context.Background()
	must(t, s.PutGroupInfo(groupInfo(tSubgroup, "Corstorphine", tCommunity, false, false)))
	admin := jidPtr(tPeer)

	cases := []struct {
		name       string
		info       events.GroupInfo
		group      string
		wantParent string
		wantEvent  groupEvent
	}{
		{
			name: "subgroup linked, seen from the community",
			info: events.GroupInfo{JID: parsed(tCommunity), Timestamp: t0, Sender: admin,
				Link: &types.GroupLinkChange{Type: types.GroupLinkChangeTypeSub, Group: *linkTarget(tHidden, "Leith", false)}},
			group: tHidden, wantParent: tCommunity,
			wantEvent: groupEvent{tCommunity, "link", tHidden, tPeer, "", t0},
		},
		{
			name: "group linked to a community, seen from the group",
			info: events.GroupInfo{JID: parsed(tStandard), Timestamp: t0, Sender: admin,
				Link: &types.GroupLinkChange{Type: types.GroupLinkChangeTypeParent, Group: *linkTarget(tCommunity, "Edinburgh", false)}},
			group: tStandard, wantParent: tCommunity,
			wantEvent: groupEvent{tCommunity, "link", tStandard, tPeer, "", t0},
		},
		{
			name: "sibling linked, parent taken from the group the event is about",
			info: events.GroupInfo{JID: parsed(tSubgroup), Timestamp: t0,
				Link: &types.GroupLinkChange{Type: types.GroupLinkChangeTypeSibling, Group: *linkTarget(tAnnounce, "News", true)}},
			group: tAnnounce, wantParent: tCommunity,
			wantEvent: groupEvent{tCommunity, "link", tAnnounce, "", "", t0},
		},
		{
			name: "subgroup unlinked",
			info: events.GroupInfo{JID: parsed(tCommunity), Timestamp: t0, Sender: admin,
				Unlink: &types.GroupLinkChange{Type: types.GroupLinkChangeTypeSub, UnlinkReason: types.GroupUnlinkReasonDefault,
					Group: *linkTarget(tHidden, "", false)}},
			group: tHidden, wantParent: "",
			wantEvent: groupEvent{tCommunity, "unlink", tHidden, tPeer, "unlinked", t0},
		},
		{
			name: "community deleted",
			info: events.GroupInfo{JID: parsed(tStandard), Timestamp: t0,
				Unlink: &types.GroupLinkChange{Type: types.GroupLinkChangeTypeParent, UnlinkReason: types.GroupUnlinkReasonDelete,
					Group: *linkTarget(tCommunity, "", false)}},
			group: tStandard, wantParent: "",
			wantEvent: groupEvent{tCommunity, "unlink", tStandard, "", "community deleted", t0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ApplyGroupLinkChanges(ctx, groupLIDs, &tc.info)
			if err != nil {
				t.Fatalf("ApplyGroupLinkChanges: %v", err)
			}
			assertGroupEvents(t, got, []groupEvent{tc.wantEvent})
			if parent := groupLink(t, s, tc.group).parent; parent != tc.wantParent {
				t.Errorf("parent of %s = %q, want %q", tc.group, parent, tc.wantParent)
			}
		})
	}

	// An unlink naming a different community leaves the stored link alone.
	stale := events.GroupInfo{JID: parsed("120363000000000999@g.us"), Timestamp: t0,
		Unlink: &types.GroupLinkChange{Type: types.GroupLinkChangeTypeSub, Group: *linkTarget(tSubgroup, "", false)}}
	if _, err := s.ApplyGroupLinkChanges(ctx, groupLIDs, &stale); err != nil {
		t.Fatal(err)
	}
	if parent := groupLink(t, s, tSubgroup).parent; parent != tCommunity {
		t.Errorf("unlink from another community cleared the link: %q", parent)
	}
}

type fakeContactGetter map[string]types.ContactInfo

func (f fakeContactGetter) GetContact(_ context.Context, jid types.JID) (types.ContactInfo, error) {
	return f[jid.String()], nil
}

func TestCommunityMember(t *testing.T) {
	contacts := fakeContactGetter{tOther: {Found: true, FullName: "Other Person"}}
	member, names := communityMember(context.Background(), groupLIDs, contacts, parsed(tLIDOther))
	if member.JID != tOther || member.PhoneNumber != "15550001002" || member.DisplayName != "Other Person" {
		t.Errorf("member = %+v", member)
	}
	if names == nil || names.JID != tOther {
		t.Errorf("learned names = %+v", names)
	}
}

func communitiesLister(jids ...string) *fakeGroupLister {
	lister := &fakeGroupLister{subGroups: map[string][]*types.GroupLinkTarget{}, failures: map[string]error{}}
	for _, jid := range jids {
		lister.joined = append(lister.joined, groupInfo(jid, "", "", true, false))
		lister.subGroups[jid] = nil
	}
	return lister
}

// Asking WhatsApp for every community's subgroups on every connect gets the
// account rate limited: the lookups are paced, a 429 ends the pass, and a
// community synced within a day is not asked again.
func TestSyncGroupLinksPacesAndStopsWhenRateLimited(t *testing.T) {
	s := newMutationStore(t)
	ctx := context.Background()
	first, second, third := "120363000000000201@g.us", "120363000000000202@g.us", "120363000000000203@g.us"
	lister := communitiesLister(first, second, third)
	lister.failures[second] = whatsmeow.ErrIQRateOverLimit
	pauses := 0

	recorded, remaining, err := syncGroupLinks(ctx, lister, s, t0, func() { pauses++ })
	if !errors.Is(err, errSubGroupsRateLimited) {
		t.Fatalf("err = %v, want rate limited", err)
	}
	if recorded != 1 || remaining != 2 || len(lister.asked) != 2 {
		t.Errorf("recorded %d, remaining %d, asked %v", recorded, remaining, lister.asked)
	}
	if pauses != 1 {
		t.Errorf("%d pauses between 2 lookups", pauses)
	}

	delete(lister.failures, second)
	lister.asked = nil
	recorded, remaining, err = syncGroupLinks(ctx, lister, s, t0.Add(time.Hour), func() {})
	if err != nil || recorded != 2 || remaining != 0 {
		t.Fatalf("retry: recorded %d, remaining %d, err %v", recorded, remaining, err)
	}
	if len(lister.asked) != 2 || lister.asked[0] == first {
		t.Errorf("retry asked %v; the community synced an hour ago should be skipped", lister.asked)
	}

	lister.asked = nil
	if _, _, err := syncGroupLinks(ctx, lister, s, t0.Add(25*time.Hour), func() {}); err != nil {
		t.Fatal(err)
	}
	if len(lister.asked) != 3 {
		t.Errorf("a day later asked %v, want all three again", lister.asked)
	}
}
