package main

// Env-gated dry-run of canonicalizeMessageStore against copies of the real
// store databases. Never run against the live files — copy them first:
//
//	cp store/messages.db store/whatsapp.db /tmp/somewhere/
//	REAL_STORE_DIR=/tmp/somewhere go test -run TestCanonicalizeRealStore -v

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestCanonicalizeRealStore(t *testing.T) {
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
	defer db.Close()

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
