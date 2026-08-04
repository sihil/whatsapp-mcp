package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/types"
)

type fakeContactStore struct {
	contacts map[types.JID]types.ContactInfo
	err      error
}

func (f fakeContactStore) GetAllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	return f.contacts, f.err
}

// newContactTestDB mirrors the production schema for the tables this file
// touches, including the foreign key that makes StoreChat's upsert matter.
func newContactTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("opening in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, ddl := range []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (
			id TEXT, chat_jid TEXT, content TEXT,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		)`,
		contactsSchema,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("creating schema: %v", err)
		}
	}
	return db
}

func contactRow(t *testing.T, db *sql.DB, jid string) (full, push, business string) {
	t.Helper()
	err := db.QueryRow(
		"SELECT full_name, push_name, business_name FROM contacts WHERE jid = ?", jid,
	).Scan(&full, &push, &business)
	if err == sql.ErrNoRows {
		return "", "", ""
	}
	if err != nil {
		t.Fatalf("reading contact %s: %v", jid, err)
	}
	return full, push, business
}

func TestIsNumericName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"", true},                  // absent is not a name
		{"16175045529", true},       // the old phone-number fallback
		{"180947643277487", true},   // a LID written into chats.name
		{"0", true},                 // 0@s.whatsapp.net
		{"Kate Sydney Cook", false}, // an actual name
		{"Group 120363158681642688", false},
		{"+1 617 504 5529", false}, // punctuation means someone chose it
		{"C3PO", false},
	} {
		if got := isNumericName(tc.name); got != tc.want {
			t.Errorf("isNumericName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A push name arriving on a message must never overwrite the address-book name.
func TestPutContactNamesNeverDowngrades(t *testing.T) {
	db := newContactTestDB(t)
	ctx := context.Background()
	jid := "15550001001@s.whatsapp.net"

	if err := putContactNames(ctx, db, jid, "Kate Sydney Cook", "Kate", ""); err != nil {
		t.Fatalf("seeding contact: %v", err)
	}
	// A later message knows only the push name.
	if err := putContactNames(ctx, db, jid, "", "katesc", ""); err != nil {
		t.Fatalf("push-name write: %v", err)
	}

	full, push, _ := contactRow(t, db, jid)
	if full != "Kate Sydney Cook" {
		t.Errorf("full_name = %q, want it preserved as %q", full, "Kate Sydney Cook")
	}
	if push != "katesc" {
		t.Errorf("push_name = %q, want the newer %q", push, "katesc")
	}

	// A business name fills its own column without disturbing the others.
	if err := putContactNames(ctx, db, jid, "", "", "Cook Consulting"); err != nil {
		t.Fatalf("business-name write: %v", err)
	}
	full, push, business := contactRow(t, db, jid)
	if full != "Kate Sydney Cook" || push != "katesc" || business != "Cook Consulting" {
		t.Errorf("got (%q, %q, %q), want all three columns intact", full, push, business)
	}
}

func TestPutContactNamesRejectsNonNames(t *testing.T) {
	db := newContactTestDB(t)
	ctx := context.Background()

	// A phone number as a "name" must not be stored at all...
	if err := putContactNames(ctx, db, "15550002002@s.whatsapp.net", "15550002002", "", ""); err != nil {
		t.Fatalf("numeric write: %v", err)
	}
	// ...nor should an all-empty write create a row.
	if err := putContactNames(ctx, db, "15550002003@s.whatsapp.net", "", "", ""); err != nil {
		t.Fatalf("empty write: %v", err)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM contacts").Scan(&rows); err != nil {
		t.Fatalf("counting contacts: %v", err)
	}
	if rows != 0 {
		t.Errorf("contacts has %d rows, want 0 — nothing written was a name", rows)
	}

	// A numeric value must not erase a real name already present.
	jid := "15550002004@s.whatsapp.net"
	if err := putContactNames(ctx, db, jid, "Glen David Roth", "", ""); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := putContactNames(ctx, db, jid, "15550002004", "", ""); err != nil {
		t.Fatalf("numeric overwrite: %v", err)
	}
	if full, _, _ := contactRow(t, db, jid); full != "Glen David Roth" {
		t.Errorf("full_name = %q, want %q", full, "Glen David Roth")
	}
}

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
	if err := putContactNames(ctx, db, "15550005001@s.whatsapp.net", "", "glenr", ""); err != nil {
		t.Fatalf("interim push name: %v", err)
	}

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

// StoreChat must not erase a known name when a later message resolves to none,
// and must not delete the messages that reference the chat.
func TestStoreChatPreservesNameAndMessages(t *testing.T) {
	db := newContactTestDB(t)
	store := &MessageStore{db: db}
	jid := "15550006001@s.whatsapp.net"
	first := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)

	if err := store.StoreChat(jid, "Kate Sydney Cook", first); err != nil {
		t.Fatalf("first StoreChat: %v", err)
	}
	if _, err := db.Exec("INSERT INTO messages (id, chat_jid, content) VALUES (?, ?, ?)", "M1", jid, "hi"); err != nil {
		t.Fatalf("inserting message: %v", err)
	}

	// A later message whose contact lookup came up empty.
	if err := store.StoreChat(jid, "", second); err != nil {
		t.Fatalf("second StoreChat: %v", err)
	}
	// ...and one that resolved only to a phone number.
	if err := store.StoreChat(jid, "15550006001", second); err != nil {
		t.Fatalf("third StoreChat: %v", err)
	}

	var name string
	var last time.Time
	if err := db.QueryRow("SELECT name, last_message_time FROM chats WHERE jid = ?", jid).Scan(&name, &last); err != nil {
		t.Fatalf("reading chat: %v", err)
	}
	if name != "Kate Sydney Cook" {
		t.Errorf("name = %q, want it preserved", name)
	}
	if !last.Equal(second) {
		t.Errorf("last_message_time = %v, want it advanced to %v", last, second)
	}

	var messages int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid = ?", jid).Scan(&messages); err != nil {
		t.Fatalf("counting messages: %v", err)
	}
	if messages != 1 {
		t.Errorf("%d messages survived the chat upsert, want 1", messages)
	}
}
