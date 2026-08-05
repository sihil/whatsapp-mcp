package main

import (
	"context"
	"database/sql"
	"testing"

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

// bestContactName is what group members and DM chats are labelled with, so a
// phone number slipping through it reaches the user as a display name.
func TestBestContactName(t *testing.T) {
	for _, tc := range []struct {
		contact types.ContactInfo
		want    string
	}{
		{types.ContactInfo{FullName: "Kate Sydney Cook", PushName: "Kate"}, "Kate Sydney Cook"},
		{types.ContactInfo{PushName: "Kate"}, "Kate"},
		{types.ContactInfo{BusinessName: "Cook Consulting"}, "Cook Consulting"},
		// A numeric entry is skipped, not returned, so a later source can win.
		{types.ContactInfo{FullName: "16175045529", PushName: "Kate"}, "Kate"},
		{types.ContactInfo{FullName: "16175045529"}, ""},
		{types.ContactInfo{}, ""},
	} {
		if got := bestContactName(tc.contact); got != tc.want {
			t.Errorf("bestContactName(%+v) = %q, want %q", tc.contact, got, tc.want)
		}
	}
}

// putNames writes contact names, failing the test on error, and reports
// whether anything was actually stored.
func putNames(t *testing.T, db *sql.DB, jid, full, push, business string) bool {
	t.Helper()
	stored, err := putContactNames(context.Background(), db, jid, full, push, business)
	if err != nil {
		t.Fatalf("putContactNames(%s, %q, %q, %q): %v", jid, full, push, business, err)
	}
	return stored
}

// A push name arriving on a message must never overwrite the address-book name.
func TestPutContactNamesNeverDowngrades(t *testing.T) {
	db := newContactTestDB(t)
	jid := "15550001001@s.whatsapp.net"

	putNames(t, db, jid, "Kate Sydney Cook", "Kate", "")
	// A later message knows only the push name.
	putNames(t, db, jid, "", "katesc", "")

	full, push, _ := contactRow(t, db, jid)
	if full != "Kate Sydney Cook" {
		t.Errorf("full_name = %q, want it preserved as %q", full, "Kate Sydney Cook")
	}
	if push != "katesc" {
		t.Errorf("push_name = %q, want the newer %q", push, "katesc")
	}

	// A business name fills its own column without disturbing the others.
	putNames(t, db, jid, "", "", "Cook Consulting")
	full, push, business := contactRow(t, db, jid)
	if full != "Kate Sydney Cook" || push != "katesc" || business != "Cook Consulting" {
		t.Errorf("got (%q, %q, %q), want all three columns intact", full, push, business)
	}
}

func TestPutContactNamesRejectsNonNames(t *testing.T) {
	db := newContactTestDB(t)

	// A phone number as a "name" must not be stored at all...
	if putNames(t, db, "15550002002@s.whatsapp.net", "15550002002", "", "") {
		t.Error("reported a numeric name as stored")
	}
	// ...nor should an all-empty write create a row.
	if putNames(t, db, "15550002003@s.whatsapp.net", "", "", "") {
		t.Error("reported an empty write as stored")
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
	putNames(t, db, jid, "Glen David Roth", "", "")
	putNames(t, db, jid, "15550002004", "", "")
	if full, _, _ := contactRow(t, db, jid); full != "Glen David Roth" {
		t.Errorf("full_name = %q, want %q", full, "Glen David Roth")
	}
}

// whatsmeow's contact store keeps FirstName alongside FullName, and app-state
// contact mutations fill them independently (PutContactName(jid, firstName,
// fullName)). bestContactName never looks at FirstName, so a contact the client
// knows by given name only is reported as having no name at all.
func TestBestContactNameUsesFirstName(t *testing.T) {
	contact := types.ContactInfo{Found: true, FirstName: "Kate"}
	if got := bestContactName(contact); got != "Kate" {
		t.Errorf("bestContactName(%+v) = %q, want %q", contact, got, "Kate")
	}
}

// The group-members endpoint writes a whole roster at once; the batch must
// apply the same rules as a single write, and roll back as a unit.
func TestStoreContactsBatch(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+dir+"/m.db?_foreign_keys=on")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(contactsSchema); err != nil {
		t.Fatalf("creating schema: %v", err)
	}
	store := &MessageStore{db: db}

	if err := store.StoreContacts(nil); err != nil {
		t.Errorf("empty batch: %v", err)
	}
	if err := store.StoreContacts([]ContactNames{
		{JID: "15550006001@s.whatsapp.net", FullName: "Kate Sydney Cook"},
		{JID: "15550006002@s.whatsapp.net", PushName: "glenr"},
		// A phone number is not a name, in a batch as anywhere else.
		{JID: "15550006003@s.whatsapp.net", FullName: "15550006003"},
	}); err != nil {
		t.Fatalf("StoreContacts: %v", err)
	}

	if full, _, _ := contactRow(t, db, "15550006001@s.whatsapp.net"); full != "Kate Sydney Cook" {
		t.Errorf("full_name = %q, want %q", full, "Kate Sydney Cook")
	}
	if _, push, _ := contactRow(t, db, "15550006002@s.whatsapp.net"); push != "glenr" {
		t.Errorf("push_name = %q, want %q", push, "glenr")
	}
	var rows int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM contacts WHERE jid = ?", "15550006003@s.whatsapp.net",
	).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 0 {
		t.Errorf("stored %d rows for a numeric-only name, want 0", rows)
	}
}

// A blank is not a name; storing one shadows a real name from a weaker source
// just as a phone number would.
func TestBlankNamesAreNotNames(t *testing.T) {
	if !isNumericName("   ") {
		t.Error("isNumericName(whitespace) = false, want true")
	}
	db := newContactTestDB(t)
	jid := "15550008001@s.whatsapp.net"
	if putNames(t, db, jid, "  ", "", "") {
		t.Error("stored a whitespace-only full name")
	}
	putNames(t, db, jid, "", "Kate", "")
	if _, push, _ := contactRow(t, db, jid); push != "Kate" {
		t.Errorf("push_name = %q, want %q", push, "Kate")
	}
	putNames(t, db, jid, "  Kate Sydney Cook  ", "", "")
	if full, _, _ := contactRow(t, db, jid); full != "Kate Sydney Cook" {
		t.Errorf("full_name = %q, want it trimmed", full)
	}
}
