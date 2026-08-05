package main

// Tests for the startup contact migration: seeding contacts from whatsmeow's
// store, clearing the phone-number chat names the old GetChatName wrote, and
// the guarantee that a restart disturbs neither. The name rules themselves are
// tested in contacts_test.go.

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/types"
)

// Contacts arrive in both namespaces; they must land under the same PN JID that
// messages.sender uses, or the read layer's join finds nothing.
func TestSyncContactsFromStoreCanonicalizesJIDs(t *testing.T) {
	db := newContactTestDB(t)
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"200000000000001@lid": pnJID("15550003001"),
	}}
	store := fakeContactStore{contacts: map[types.JID]types.ContactInfo{
		lidJID("200000000000001"):                     {FullName: "Benjamin Black"},
		pnJID("15550003002"):                          {PushName: "Russ"},
		{User: "15550003003", Server: legacyPNServer}: {FullName: "Sim Blaustein"},
		pnJID("15550003004"):                          {FullName: "15550003004"}, // not a name
	}}

	synced, err := syncContactsFromStore(context.Background(), db, store, lids)
	if err != nil {
		t.Fatalf("syncing: %v", err)
	}
	if synced != 3 {
		t.Errorf("synced = %d, want 3 (the fourth carries only digits)", synced)
	}

	for jid, want := range map[string]string{
		"15550003001@s.whatsapp.net": "Benjamin Black", // LID mapped to PN
		"15550003003@s.whatsapp.net": "Sim Blaustein",  // legacy @c.us normalized
	} {
		if full, _, _ := contactRow(t, db, jid); full != want {
			t.Errorf("contacts[%s].full_name = %q, want %q", jid, full, want)
		}
	}
	if _, push, _ := contactRow(t, db, "15550003002@s.whatsapp.net"); push != "Russ" {
		t.Errorf("push_name = %q, want %q", push, "Russ")
	}
	if full, _, _ := contactRow(t, db, "15550003004@s.whatsapp.net"); full != "" {
		t.Errorf("stored %q as a name, want the digits rejected", full)
	}
}

// whatsmeow keeps one contact row per JID it has seen, so the same person has
// both a PN-keyed row (the address book, via PutContactName) and a LID-keyed
// row (learned from wire traffic, via PutPushName) — GetAllContacts returns
// both. canonicalContactJID collapses them onto one key, so the two rows fight
// over the same column and Go's randomized map iteration order picks the
// winner. The address-book name must win every time, not most of the time.
func TestSyncContactsFromStoreMergesBothNamespaces(t *testing.T) {
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"200000000000009@lid": pnJID("15550003009"),
	}}
	// Both rows are the same person. The PN row is the address book; the LID
	// row carries a push name the client learned before it knew the mapping.
	contacts := map[types.JID]types.ContactInfo{
		pnJID("15550003009"):      {FullName: "Benjamin Black", PushName: "Ben"},
		lidJID("200000000000009"): {PushName: "bennyb"},
	}

	for run := 0; run < 50; run++ {
		db := newContactTestDB(t)
		if _, err := syncContactsFromStore(
			context.Background(), db, fakeContactStore{contacts: contacts}, lids,
		); err != nil {
			t.Fatalf("run %d: syncing: %v", run, err)
		}
		full, push, _ := contactRow(t, db, "15550003009@s.whatsapp.net")
		if full != "Benjamin Black" {
			t.Fatalf("run %d: full_name = %q, want the address-book name %q "+
				"(the LID-keyed row won the merge)", run, full, "Benjamin Black")
		}
		if push == "" {
			t.Fatalf("run %d: push_name = %q, want a push name from one of the two rows", run, push)
		}
		db.Close()
	}
}

// The same merge with both rows holding a full name: whichever the map hands
// over last is stored, so the name a chat shows can change across restarts
// without anything on WhatsApp's side changing.
func TestSyncContactsFromStoreIsDeterministic(t *testing.T) {
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"200000000000010@lid": pnJID("15550003010"),
	}}
	contacts := map[types.JID]types.ContactInfo{
		pnJID("15550003010"):      {FullName: "Benjamin Black"},
		lidJID("200000000000010"): {FullName: "B"},
	}

	seen := map[string]int{}
	for run := 0; run < 50; run++ {
		db := newContactTestDB(t)
		if _, err := syncContactsFromStore(
			context.Background(), db, fakeContactStore{contacts: contacts}, lids,
		); err != nil {
			t.Fatalf("run %d: syncing: %v", run, err)
		}
		full, _, _ := contactRow(t, db, "15550003010@s.whatsapp.net")
		seen[full]++
		db.Close()
	}
	if len(seen) > 1 {
		t.Errorf("50 identical syncs produced %d different names: %v", len(seen), seen)
	}
}
func TestScrubNumericChatNames(t *testing.T) {
	db := newContactTestDB(t)
	seed := map[string]string{
		"15550004001@s.whatsapp.net": "16175045529",     // phone number as name
		"15550004002@s.whatsapp.net": "180947643277487", // LID as name
		"0@s.whatsapp.net":           "0",
		"15550004003@s.whatsapp.net": "Kate Sydney Cook", // keep
		"120363158681642688@g.us":    "MIT Sloan LFM/MBA 2008",
		"120363158681642689@g.us":    "Group 120363158681642689", // keep: readable
	}
	for jid, name := range seed {
		if _, err := db.Exec("INSERT INTO chats (jid, name) VALUES (?, ?)", jid, name); err != nil {
			t.Fatalf("seeding chats: %v", err)
		}
	}

	scrubbed, err := scrubNumericChatNames(context.Background(), db)
	if err != nil {
		t.Fatalf("scrubbing: %v", err)
	}
	if scrubbed != 3 {
		t.Errorf("scrubbed = %d, want 3", scrubbed)
	}

	rows, err := db.Query("SELECT jid, name FROM chats WHERE name IS NOT NULL ORDER BY jid")
	if err != nil {
		t.Fatalf("reading chats: %v", err)
	}
	defer rows.Close()
	kept := map[string]string{}
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		kept[jid] = name
	}
	if len(kept) != 3 {
		t.Errorf("%d names survived, want 3: %v", len(kept), kept)
	}
	for _, jid := range []string{"15550004003@s.whatsapp.net", "120363158681642688@g.us", "120363158681642689@g.us"} {
		if kept[jid] != seed[jid] {
			t.Errorf("chats[%s].name = %q, want %q", jid, kept[jid], seed[jid])
		}
	}

	// Second pass finds nothing left to do.
	if again, err := scrubNumericChatNames(context.Background(), db); err != nil || again != 0 {
		t.Errorf("second scrub = (%d, %v), want (0, nil)", again, err)
	}
}

// A bridge restart must not disturb what the previous run established.
func TestMigrateContactsIsIdempotent(t *testing.T) {
	db := newContactTestDB(t)
	ctx := context.Background()
	lids := fakeLIDMapper{}
	store := fakeContactStore{contacts: map[types.JID]types.ContactInfo{
		pnJID("15550005001"): {FullName: "Glen David Roth", PushName: "Glen Roth"},
	}}
	if _, err := db.Exec(
		"INSERT INTO chats (jid, name) VALUES (?, ?)", "15550005001@s.whatsapp.net", "15550005001",
	); err != nil {
		t.Fatalf("seeding chats: %v", err)
	}

	synced, scrubbed, err := migrateContacts(ctx, db, store, lids)
	if err != nil {
		t.Fatalf("first migration: %v", err)
	}
	if synced != 1 || scrubbed != 1 {
		t.Fatalf("first migration = (%d synced, %d scrubbed), want (1, 1)", synced, scrubbed)
	}

	// A push name learned between restarts must survive the next sync.
	putNames(t, db, "15550005001@s.whatsapp.net", "", "glenr", "")

	synced, scrubbed, err = migrateContacts(ctx, db, store, lids)
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if scrubbed != 0 {
		t.Errorf("second migration scrubbed %d rows, want 0", scrubbed)
	}
	full, push, _ := contactRow(t, db, "15550005001@s.whatsapp.net")
	if full != "Glen David Roth" {
		t.Errorf("full_name = %q, want %q", full, "Glen David Roth")
	}
	if push != "Glen Roth" {
		t.Errorf("push_name = %q, want the contact store's %q to win on re-sync", push, "Glen Roth")
	}
	var chatName sql.NullString
	if err := db.QueryRow(
		"SELECT name FROM chats WHERE jid = ?", "15550005001@s.whatsapp.net",
	).Scan(&chatName); err != nil {
		t.Fatalf("reading chat: %v", err)
	}
	if chatName.Valid {
		t.Errorf("chats.name = %q, want NULL", chatName.String)
	}
}

// A group name is user-set and has no contact behind it, so an all-digit group
// name is real. Clearing it would destroy the only copy.
func TestGroupNamesAreExemptFromTheNumericRule(t *testing.T) {
	if got := keepChatName("120363158681642688@g.us", "2024"); got != "2024" {
		t.Errorf("keepChatName(group, %q) = %q, want it kept", "2024", got)
	}
	if got := keepChatName("15550007001@s.whatsapp.net", "15550007001"); got != "" {
		t.Errorf("keepChatName(dm, phone number) = %q, want it dropped", got)
	}
	if got := keepChatName("120363158681642688@g.us", "  Hikers  "); got != "Hikers" {
		t.Errorf("keepChatName(group) = %q, want it trimmed", got)
	}

	db := newContactTestDB(t)
	for jid, name := range map[string]string{
		"120363158681642688@g.us":    "2024",        // a real group name
		"15550007002@s.whatsapp.net": "15550007002", // the old phone-number fallback
	} {
		if _, err := db.Exec("INSERT INTO chats (jid, name) VALUES (?, ?)", jid, name); err != nil {
			t.Fatalf("seeding %s: %v", jid, err)
		}
	}
	scrubbed, err := scrubNumericChatNames(context.Background(), db)
	if err != nil {
		t.Fatalf("scrubbing: %v", err)
	}
	if scrubbed != 1 {
		t.Errorf("scrubbed %d rows, want 1 (the group must be left alone)", scrubbed)
	}
	var groupName sql.NullString
	if err := db.QueryRow(
		"SELECT name FROM chats WHERE jid = ?", "120363158681642688@g.us",
	).Scan(&groupName); err != nil {
		t.Fatalf("reading group: %v", err)
	}
	if groupName.String != "2024" {
		t.Errorf("group name = %q, want %q — the scrub destroyed it", groupName.String, "2024")
	}
}
