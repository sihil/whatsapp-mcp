package main

// Contact name storage for messages.db.
//
// The read layer resolves a message's sender by JID, but chats only holds a row
// per *chat* — so a group participant is nameless unless they also have a DM
// with the user. whatsmeow already knows these names (its contact store is
// populated from the address book and from push names seen on the wire); this
// file copies them into messages.db so the Python side can join against them
// without a network call and without depending on whatsmeow's private schema.
//
// Names never regress: every writer upserts only the columns it actually knows,
// so a push name arriving with a message cannot overwrite an address-book name.
// A name that is nothing but digits is not a name — it is the phone number the
// caller failed to resolve — and is rejected at every write.

import (
	"context"
	"database/sql"

	"go.mau.fi/whatsmeow/types"
)

// contactLister is the subset of whatsmeow's contact store used to seed the
// contacts table.
type contactLister interface {
	GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)
}

// execer covers both *sql.DB and *sql.Tx so the writes below can join a
// migration transaction or stand alone.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const contactsSchema = `
	CREATE TABLE IF NOT EXISTS contacts (
		jid           TEXT PRIMARY KEY,
		full_name     TEXT NOT NULL DEFAULT '',
		push_name     TEXT NOT NULL DEFAULT '',
		business_name TEXT NOT NULL DEFAULT ''
	)`

// upsertContact writes only the non-empty columns, leaving the rest of the row
// as it was. Callers hold different-quality data — the address book knows a
// full name, a message stanza knows only a push name — and whichever arrives
// last must not erase what the others learned.
const upsertContact = `
	INSERT INTO contacts (jid, full_name, push_name, business_name) VALUES (?, ?, ?, ?)
	ON CONFLICT(jid) DO UPDATE SET
		full_name     = CASE WHEN excluded.full_name     != '' THEN excluded.full_name     ELSE contacts.full_name     END,
		push_name     = CASE WHEN excluded.push_name     != '' THEN excluded.push_name     ELSE contacts.push_name     END,
		business_name = CASE WHEN excluded.business_name != '' THEN excluded.business_name ELSE contacts.business_name END`

// isNumericName reports whether a name carries no information beyond digits.
// GetChatName used to fall back to a bare phone number, and those strings are
// still in the store; they defeat the read layer's fallback chain by looking
// like a successful lookup. An empty string counts as numeric: neither is a name.
func isNumericName(name string) bool {
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// keepName blanks a name that is not worth storing.
func keepName(name string) string {
	if isNumericName(name) {
		return ""
	}
	return name
}

// canonicalContactJID puts a contact JID in the same namespace as
// messages.sender (see identity.go): full user@server, phone-number form
// wherever the LID mapping is known.
func canonicalContactJID(ctx context.Context, lids lidMapper, jid types.JID) types.JID {
	return canonicalChatJID(ctx, lids, normalizeLegacyServer(jid.ToNonAD()), types.EmptyJID)
}

// putContactNames records whichever names the caller knows. Writing a row with
// nothing but digits (or nothing at all) is a no-op rather than an empty row.
func putContactNames(ctx context.Context, ex execer, jid, fullName, pushName, businessName string) error {
	fullName, pushName, businessName = keepName(fullName), keepName(pushName), keepName(businessName)
	if jid == "" || (fullName == "" && pushName == "" && businessName == "") {
		return nil
	}
	_, err := ex.ExecContext(ctx, upsertContact, jid, fullName, pushName, businessName)
	return err
}

// StoreContactNames records a contact's names, e.g. from a group participant
// lookup. Safe to call with partial information.
func (store *MessageStore) StoreContactNames(jid, fullName, pushName, businessName string) error {
	return putContactNames(context.Background(), store.db, jid, fullName, pushName, businessName)
}

// StorePushName records the display name a sender chose for themselves, which
// rides along on every incoming message. It is the only name available for
// someone who is not in the address book.
func (store *MessageStore) StorePushName(jid, pushName string) error {
	return putContactNames(context.Background(), store.db, jid, "", pushName, "")
}

// syncContactsFromStore copies whatsmeow's contact store into contacts.
// Idempotent: re-running picks up names the client has learned since, and
// re-writing an unchanged name is a no-op.
func syncContactsFromStore(ctx context.Context, ex execer, contacts contactLister, lids lidMapper) (int, error) {
	all, err := contacts.GetAllContacts(ctx)
	if err != nil {
		return 0, err
	}
	written := 0
	for jid, info := range all {
		canonical := canonicalContactJID(ctx, lids, jid).String()
		before := written
		if keepName(info.FullName) != "" || keepName(info.PushName) != "" || keepName(info.BusinessName) != "" {
			written++
		}
		if err := putContactNames(ctx, ex, canonical, info.FullName, info.PushName, info.BusinessName); err != nil {
			return before, err
		}
	}
	return written, nil
}

// scrubNumericChatNames clears chat names that are bare phone numbers, written
// by the pre-fix GetChatName fallback. They are worse than no name: the read
// layer cannot tell them from a real one, so it stops its fallback chain on
// them. NULL lets the read layer fall through to contacts instead.
func scrubNumericChatNames(ctx context.Context, ex execer) (int, error) {
	res, err := ex.ExecContext(ctx, `
		UPDATE chats SET name = NULL
		WHERE name IS NOT NULL AND name NOT GLOB '*[^0-9]*'`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// migrateContacts runs at every bridge startup, beside canonicalizeMessageStore.
// Creating the table, seeding it and scrubbing the old junk names are all
// idempotent, and all no-ops against a fresh store.
func migrateContacts(ctx context.Context, db *sql.DB, contacts contactLister, lids lidMapper) (synced int, scrubbed int, err error) {
	if _, err = db.ExecContext(ctx, contactsSchema); err != nil {
		return 0, 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	if synced, err = syncContactsFromStore(ctx, tx, contacts, lids); err != nil {
		return 0, 0, err
	}
	if scrubbed, err = scrubNumericChatNames(ctx, tx); err != nil {
		return 0, 0, err
	}
	return synced, scrubbed, tx.Commit()
}
