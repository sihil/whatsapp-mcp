package main

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// A logged-out or never-paired session gets a device from NewDevice, whose
// stores are nil until pairing. The startup migration must not crash on it,
// or the bridge dies before it can print the QR code to re-pair.
func TestMigrateContactsUnpairedDevice(t *testing.T) {
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3", "file::memory:?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatalf("opening whatsmeow store: %v", err)
	}
	device := container.NewDevice()

	db := newContactTestDB(t)
	if _, err := db.Exec(
		"INSERT INTO chats (jid, name) VALUES (?, ?)", "15550009001@s.whatsapp.net", "15550009001",
	); err != nil {
		t.Fatalf("seeding chats: %v", err)
	}

	synced, scrubbed, err := migrateContacts(ctx, db, device.Contacts, device.LIDs)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if synced != 0 || scrubbed != 1 {
		t.Errorf("migration = (%d synced, %d scrubbed), want (0, 1)", synced, scrubbed)
	}
}
