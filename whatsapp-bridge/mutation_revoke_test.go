package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestRevokeInGroupNeedsConfirmedAdmin(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "G1", tChatGrp, tPeer, "spam", "", false)
	notAdmin := func() bool { return false }
	for name, check := range map[string]adminCheck{"no check": nil, "not an admin": notAdmin} {
		if ok, _ := s.ApplyRevoke(tChatGrp, "G1", tOther, check, t0); ok {
			t.Fatalf("%s: another participant deleted a group message", name)
		}
	}
	if ok, err := s.ApplyRevoke(tChatGrp, "G1", tOther, func() bool { return true }, t0); !ok || err != nil {
		t.Fatalf("group revoke by an admin: %v %v", ok, err)
	}
	if _, _, deleted, _, _ := msgState(t, s, "G1", tChatGrp); !deleted {
		t.Error("not tombstoned")
	}
}

func TestRevokeInDMIgnoresAdminCheck(t *testing.T) {
	s := newMutationStore(t)
	putMsg(t, s, "D9", tChatDM, tPeer, "hi", "", false)
	if ok, _ := s.ApplyRevoke(tChatDM, "D9", tOther, func() bool { return true }, t0); ok {
		t.Error("a non-author revoke was applied in a DM")
	}
}

func TestIsAdminIn(t *testing.T) {
	pn := types.NewJID("15550001111", types.DefaultUserServer)
	lid := types.NewJID("987654321", types.HiddenUserServer)
	roster := []types.GroupParticipant{
		{JID: lid, LID: lid, PhoneNumber: pn, IsAdmin: true},
		{JID: types.NewJID("15550002222", types.DefaultUserServer)},
	}
	if !isAdminIn(roster, pn) || !isAdminIn(roster, lid) {
		t.Error("admin not recognised under both its PN and LID spellings")
	}
	if isAdminIn(roster, types.NewJID("15550002222", types.DefaultUserServer)) {
		t.Error("plain member reported as admin")
	}
	if isAdminIn(roster, types.NewJID("15550003333", types.DefaultUserServer)) {
		t.Error("non-member reported as admin")
	}
}
