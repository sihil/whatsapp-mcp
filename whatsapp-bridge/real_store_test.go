package main

// Env-gated dry-runs of the startup migrations against copies of the real
// store databases. Never run against the live files — copy them first:
//
//	cp store/messages.db store/whatsapp.db /tmp/somewhere/
//	REAL_STORE_DIR=/tmp/somewhere go test -run RealStore -v

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// openRealStore opens the copied databases and returns the device behind them.
func openRealStore(t *testing.T) (*sql.DB, *store.Device) {
	t.Helper()
	dir := os.Getenv("REAL_STORE_DIR")
	if dir == "" {
		t.Skip("REAL_STORE_DIR not set; skipping real-store migration dry-run")
	}

	container, err := sqlstore.New(context.Background(), "sqlite3",
		"file:"+filepath.Join(dir, "whatsapp.db")+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatalf("opening whatsapp.db copy: %v", err)
	}
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		t.Fatalf("getting device: %v", err)
	}

	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("opening messages.db copy: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, device
}

// TestMigrateContactsRealStore reports how much of the real store's group
// traffic becomes attributable, and asserts the migration converges.
func TestMigrateContactsRealStore(t *testing.T) {
	db, device := openRealStore(t)
	ctx := context.Background()

	countQuery := func(q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("counting (%s): %v", q, err)
		}
		return n
	}
	// A sender is attributable when some table holds a name for it that is not
	// just digits — exactly what the Python read layer looks for.
	const attributable = `
		SELECT COUNT(*) FROM messages m
		WHERE m.chat_jid LIKE '%@g.us' AND m.is_from_me = 0 AND EXISTS (
			SELECT 1 FROM contacts ct WHERE ct.jid = m.sender AND (
				ct.full_name GLOB '*[^0-9]*' OR ct.push_name GLOB '*[^0-9]*'
				OR ct.business_name GLOB '*[^0-9]*')
			UNION ALL
			SELECT 1 FROM chats ch WHERE ch.jid = m.sender AND ch.name GLOB '*[^0-9]*')`
	const groupMessages = `SELECT COUNT(*) FROM messages
		WHERE chat_jid LIKE '%@g.us' AND is_from_me = 0`
	const numericChatNames = `SELECT COUNT(*) FROM chats
		WHERE name IS NOT NULL AND name NOT GLOB '*[^0-9]*'
		AND jid NOT LIKE '%@g.us' AND jid NOT LIKE '%@broadcast'
		AND jid NOT LIKE '%@newsletter'`

	if _, err := db.Exec(contactsSchema); err != nil {
		t.Fatalf("creating contacts table: %v", err)
	}
	before := countQuery(attributable)
	total := countQuery(groupMessages)
	junkBefore := countQuery(numericChatNames)

	synced, scrubbed, err := migrateContacts(ctx, db, device.Contacts, device.LIDs)
	if err != nil {
		t.Fatalf("migrateContacts: %v", err)
	}
	after := countQuery(attributable)

	t.Logf("synced %d contacts, cleared %d phone-number chat names (of %d)", synced, scrubbed, junkBefore)
	t.Logf("group messages attributable: %d/%d before -> %d/%d after", before, total, after, total)
	if after <= before {
		t.Errorf("attribution did not improve: %d -> %d", before, after)
	}
	if n := countQuery(numericChatNames); n != 0 {
		t.Errorf("%d phone-number chat names remain", n)
	}

	// A restart must not undo the first run's work.
	if _, rescrubbed, err := migrateContacts(ctx, db, device.Contacts, device.LIDs); err != nil {
		t.Fatalf("second migrateContacts: %v", err)
	} else if rescrubbed != 0 {
		t.Errorf("second run scrubbed %d rows, want 0 (not idempotent)", rescrubbed)
	}
	if again := countQuery(attributable); again != after {
		t.Errorf("attribution changed on re-run: %d -> %d", after, again)
	}
}

func TestCanonicalizeRealStore(t *testing.T) {
	db, device := openRealStore(t)

	changed, err := canonicalizeMessageStore(context.Background(), db, device.LIDs, device.GetJID())
	if err != nil {
		t.Fatalf("canonicalizeMessageStore: %v", err)
	}
	t.Logf("first run changed %d rows", changed)

	countQuery := func(q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("counting (%s): %v", q, err)
		}
		return n
	}

	if n := countQuery(`SELECT COUNT(*) FROM messages WHERE sender NOT LIKE '%@%'`); n != 0 {
		t.Errorf("%d bare-digit senders remain after migration", n)
	}
	if n := countQuery(`SELECT COUNT(*) FROM messages m LEFT JOIN chats c ON m.chat_jid = c.jid WHERE c.jid IS NULL`); n != 0 {
		t.Errorf("%d orphaned messages (chat_jid without a chats row)", n)
	}
	own := device.GetJID().ToNonAD().String()
	if n := countQuery(`SELECT COUNT(*) FROM messages WHERE is_from_me = 1 AND sender <> '` + own + `'`); n != 0 {
		t.Errorf("%d is_from_me rows do not carry the own JID", n)
	}
	t.Logf("remaining @lid-keyed chats (no PN mapping known): %d",
		countQuery(`SELECT COUNT(*) FROM chats WHERE jid LIKE '%@lid'`))
	t.Logf("remaining @lid senders (no PN mapping known): %d",
		countQuery(`SELECT COUNT(*) FROM messages WHERE sender LIKE '%@lid'`))

	changed, err = canonicalizeMessageStore(context.Background(), db, device.LIDs, device.GetJID())
	if err != nil {
		t.Fatalf("second canonicalizeMessageStore: %v", err)
	}
	if changed != 0 {
		t.Errorf("second run changed %d rows, want 0 (not idempotent)", changed)
	}
}
