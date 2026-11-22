package main

import (
	"context"
	"database/sql"

	"go.mau.fi/whatsmeow/types"
)

// group_participants records who is in each group the account is in, and,
// keyed by the community's own JID, everyone across a community's groups
// (GetLinkedGroupsParticipants), which covers its groups the account is not
// in. The MCP server's memberDMs rule reads it to decide whose DMs an
// instance may read. Participants follow the messages.sender rule: phone
// number JID when known, else the LID.
const groupParticipantsSchema = `
	CREATE TABLE IF NOT EXISTS group_participants (
		group_jid TEXT NOT NULL,
		participant TEXT NOT NULL,
		PRIMARY KEY (group_jid, participant)
	);
	CREATE INDEX IF NOT EXISTS group_participants_participant ON group_participants(participant)`

func participantJID(ctx context.Context, lids lidMapper, p types.GroupParticipant) string {
	if !p.PhoneNumber.IsEmpty() {
		return p.PhoneNumber.ToNonAD().String()
	}
	return canonicalContactJID(ctx, lids, p.JID).String()
}

// PutGroupParticipants replaces the stored members of group.
func (store *MessageStore) PutGroupParticipants(group string, participants []string) error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM group_participants WHERE group_jid = ?`, group); err != nil {
		return err
	}
	for _, p := range participants {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO group_participants (group_jid, participant) VALUES (?, ?)`,
			group, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ApplyParticipantChanges keeps group members current between syncs from a
// live event's joins and leaves. Community-wide rows are left to the daily
// sync: leaving one group does not mean leaving the community.
func (store *MessageStore) ApplyParticipantChanges(evts []groupEvent) error {
	for _, e := range evts {
		var err error
		switch e.kind {
		case "join":
			_, err = store.db.Exec(`INSERT OR IGNORE INTO group_participants (group_jid, participant) VALUES (?, ?)`,
				e.chat, e.participant)
		case "leave":
			_, err = store.db.Exec(`DELETE FROM group_participants WHERE group_jid = ? AND participant = ?`,
				e.chat, e.participant)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// moveParticipantIdentity rewrites one person's JID in group_participants;
// a group already listing the new JID keeps its one row.
func moveParticipantIdentity(ctx context.Context, tx *sql.Tx, oldJID, newJID string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO group_participants (group_jid, participant)
		SELECT group_jid, ? FROM group_participants WHERE participant = ?`, newJID, oldJID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM group_participants WHERE participant = ?`, oldJID)
	return err
}

func canonicalizeGroupParticipants(ctx context.Context, tx *sql.Tx, lids lidMapper) error {
	stored, err := collectColumn(tx, `SELECT DISTINCT participant FROM group_participants`)
	if err != nil {
		return err
	}
	for _, jid := range stored {
		canonical, ok := canonicalStoredSender(ctx, lids, jid)
		if !ok {
			continue
		}
		if err := moveParticipantIdentity(ctx, tx, jid, canonical); err != nil {
			return err
		}
	}
	return nil
}
