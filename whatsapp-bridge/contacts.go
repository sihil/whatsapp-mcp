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
	"sort"
	"strings"

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
// like a successful lookup. Empty and whitespace-only count as numeric: a blank
// is not a name either, and storing one shadows a real name just as badly.
func isNumericName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// keepName blanks a name that is not worth storing, and trims the rest.
func keepName(name string) string {
	if isNumericName(name) {
		return ""
	}
	return strings.TrimSpace(name)
}

// keepChatName applies the same rule to a chat's name, exempting groups: a
// group name is chosen by a person and has no contact behind it to fall back
// to, so digits there are a real name rather than a failed lookup.
func keepChatName(jid, name string) string {
	if strings.HasSuffix(jid, "@"+types.GroupServer) {
		return strings.TrimSpace(name)
	}
	return keepName(name)
}

// bestContactName picks the name to show for a contact, best source first, and
// returns "" when the store knows nothing worth showing. This is the display
// precedence — the same order contact_name_expr() applies on the Python side
// (see whatsapp-mcp-server/db_names.py) — and every caller must go through it
// rather than testing the fields directly, or it will hand back a phone number.
//
// FirstName sits behind FullName because whatsmeow fills the two independently
// (store.PutContactName takes both), so a contact the client knows only by
// given name has an empty FullName and would otherwise look nameless.
func bestContactName(contact types.ContactInfo) string {
	for _, candidate := range []string{
		contact.FullName, contact.FirstName, contact.PushName, contact.BusinessName,
	} {
		if name := keepName(candidate); name != "" {
			return name
		}
	}
	return ""
}

// ContactNames is one person's names as a single source knows them.
type ContactNames struct {
	JID          string
	FullName     string
	PushName     string
	BusinessName string
}

// contactNamesOf maps whatsmeow's record onto the three stored columns, with
// FirstName backing full_name for the reason above.
func contactNamesOf(jid string, info types.ContactInfo) ContactNames {
	full := info.FullName
	if keepName(full) == "" {
		full = info.FirstName
	}
	return ContactNames{
		JID:          jid,
		FullName:     full,
		PushName:     info.PushName,
		BusinessName: info.BusinessName,
	}
}

// canonicalContactJID puts a contact JID in the same namespace as
// messages.sender (see identity.go): full user@server, phone-number form
// wherever the LID mapping is known.
func canonicalContactJID(ctx context.Context, lids lidMapper, jid types.JID) types.JID {
	return canonicalChatJID(ctx, lids, normalizeLegacyServer(jid.ToNonAD()), types.EmptyJID)
}

// putContactNames records whichever names the caller knows, reporting whether
// there was anything worth writing. Writing a row with nothing but digits (or
// nothing at all) is a no-op rather than an empty row.
func putContactNames(ctx context.Context, ex execer, jid, fullName, pushName, businessName string) (bool, error) {
	fullName, pushName, businessName = keepName(fullName), keepName(pushName), keepName(businessName)
	if jid == "" || (fullName == "" && pushName == "" && businessName == "") {
		return false, nil
	}
	_, err := ex.ExecContext(ctx, upsertContact, jid, fullName, pushName, businessName)
	return err == nil, err
}

// StoreContactNames records a contact's names, e.g. from a group participant
// lookup. Safe to call with partial information.
func (store *MessageStore) StoreContactNames(jid, fullName, pushName, businessName string) error {
	_, err := putContactNames(context.Background(), store.db, jid, fullName, pushName, businessName)
	return err
}

// StorePushName records the display name a sender chose for themselves, which
// rides along on every incoming message. It is the only name available for
// someone who is not in the address book.
func (store *MessageStore) StorePushName(jid, pushName string) error {
	_, err := putContactNames(context.Background(), store.db, jid, "", pushName, "")
	return err
}

// StoreContacts records a batch in one transaction. A group roster is the one
// place the bridge learns names in bulk, and writing them one at a time costs
// a separate fsync per participant — a large group spends more time committing
// than fetching.
func (store *MessageStore) StoreContacts(contacts []ContactNames) error {
	if len(contacts) == 0 {
		return nil
	}
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, contact := range contacts {
		if _, err := putContactNames(
			ctx, tx, contact.JID, contact.FullName, contact.PushName, contact.BusinessName,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// syncContactsFromStore copies whatsmeow's contact store into contacts.
// Idempotent: re-running picks up names the client has learned since, and
// re-writing an unchanged name is a no-op.
func syncContactsFromStore(ctx context.Context, ex execer, contacts contactLister, lids lidMapper) (int, error) {
	all, err := contacts.GetAllContacts(ctx)
	if err != nil {
		return 0, err
	}
	// Two entries — one LID-keyed, one PN-keyed — can canonicalize to the same
	// JID, and the upsert lets the last non-empty write win. Go randomizes map
	// iteration, so without a fixed order such a contact's name flips between
	// bridge restarts. Write PN-keyed entries last: that is the address-book
	// record for the person, so it is the one that should win.
	jids := make([]types.JID, 0, len(all))
	for jid := range all {
		jids = append(jids, jid)
	}
	// Classify with the normalized server, the same way canonicalContactJID
	// does: @c.us is the pre-multidevice spelling of the phone-number server,
	// so a legacy address-book row is PN-keyed and must not be sorted into the
	// bucket that loses to LID rows.
	isPN := func(jid types.JID) bool {
		return normalizeLegacyServer(jid.ToNonAD()).Server == types.DefaultUserServer
	}
	sort.Slice(jids, func(i, j int) bool {
		if iPN, jPN := isPN(jids[i]), isPN(jids[j]); iPN != jPN {
			return !iPN
		}
		return jids[i].String() < jids[j].String()
	})

	written := 0
	for _, jid := range jids {
		names := contactNamesOf(canonicalContactJID(ctx, lids, jid).String(), all[jid])
		stored, err := putContactNames(
			ctx, ex, names.JID, names.FullName, names.PushName, names.BusinessName,
		)
		if err != nil {
			return written, err
		}
		if stored {
			written++
		}
	}
	return written, nil
}

// scrubNumericChatNames clears chat names that are bare phone numbers, written
// by the pre-fix GetChatName fallback. They are worse than no name: the read
// layer cannot tell them from a real one, so it stops its fallback chain on
// them. NULL lets the read layer fall through to contacts instead.
//
// Groups are exempt. A group name is chosen by a person and there is no contact
// behind a @g.us JID to fall back to, so digits there are a real name — "2024"
// is a plausible group name, and clearing it would destroy the only copy.
func scrubNumericChatNames(ctx context.Context, ex execer) (int, error) {
	res, err := ex.ExecContext(ctx, `
		UPDATE chats SET name = NULL
		WHERE name IS NOT NULL AND jid NOT LIKE '%@g.us'
		  AND TRIM(name) NOT GLOB '*[^0-9]*'`)
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
