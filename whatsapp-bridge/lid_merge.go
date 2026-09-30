package main

// Folding an @lid identity into its phone number as soon as the mapping is
// learned. Rows written while the PN<->LID mapping was unknown keep the @lid
// spelling (see identity.go). The startup canonicalization rewrites them, but
// until then one person reads as two senders, and an edit, revoke or changed
// reaction that names them by phone number does not match their earlier rows.

import (
	"context"
	"database/sql"

	"go.mau.fi/whatsmeow/types"
)

// senderIndexSchema lets a merge find one sender's rows without scanning
// messages, which it does for every newly learned mapping.
const senderIndexSchema = `CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender)`

// rowCounter sums RowsAffected over a series of statements.
type rowCounter int

func (c *rowCounter) add(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	*c += rowCounter(n)
	return err
}

// mergeChatKey folds the chat row and messages under oldKey into newKey,
// leaving an alias behind, and reports the rows changed. The same message id
// can exist under both keys (captured live under one namespace,
// history-synced under the other), so moves are conflict-aware: the row
// already under newKey wins. The timestamp column holds TEXT with mixed local
// offsets, so comparison must go through datetime(), not raw MAX().
func mergeChatKey(ctx context.Context, tx *sql.Tx, oldKey, newKey string) (int, error) {
	var n rowCounter
	if err := n.add(tx.ExecContext(ctx, `
		INSERT INTO jid_aliases (old_jid, new_jid) VALUES (?, ?)
		ON CONFLICT(old_jid) DO UPDATE SET new_jid = excluded.new_jid`,
		oldKey, newKey)); err != nil {
		return 0, err
	}
	if err := n.add(tx.ExecContext(ctx, `
		INSERT INTO chats (jid, name, last_message_time)
		SELECT ?, name, last_message_time FROM chats WHERE jid = ?
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN (chats.name IS NULL OR chats.name = '') AND excluded.name <> '' THEN excluded.name ELSE chats.name END,
			last_message_time = CASE
				WHEN chats.last_message_time IS NULL THEN excluded.last_message_time
				WHEN excluded.last_message_time IS NOT NULL
					AND datetime(excluded.last_message_time) > datetime(chats.last_message_time)
					THEN excluded.last_message_time
				ELSE chats.last_message_time
			END`,
		newKey, oldKey)); err != nil {
		return 0, err
	}
	if err := n.add(tx.ExecContext(ctx, `UPDATE OR IGNORE messages SET chat_jid = ? WHERE chat_jid = ?`, newKey, oldKey)); err != nil {
		return 0, err
	}
	if err := n.add(tx.ExecContext(ctx, `DELETE FROM messages WHERE chat_jid = ?`, oldKey)); err != nil {
		return 0, err
	}
	if err := moveColumnValue(ctx, tx, "chat_jid", oldKey, newKey); err != nil {
		return 0, err
	}
	err := n.add(tx.ExecContext(ctx, `DELETE FROM chats WHERE jid = ?`, oldKey))
	return int(n), err
}

// mergeLID moves everything stored under lid to pn: the messages and
// reactions sent under it, and a DM chat keyed by it.
func mergeLID(ctx context.Context, tx *sql.Tx, lid, pn string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET sender = ? WHERE sender = ?`, pn, lid); err != nil {
		return err
	}
	if err := moveColumnValue(ctx, tx, "sender", lid, pn); err != nil {
		return err
	}
	var chats int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats WHERE jid = ?`, lid).Scan(&chats); err != nil || chats == 0 {
		return err
	}
	_, err := mergeChatKey(ctx, tx, lid, pn)
	return err
}

// MergeLIDs applies mergeLID to each lid->pn pair (keyed by the LID string)
// this process has not merged yet, in one transaction. Merged LIDs are
// remembered, so a sender seen before costs a map lookup.
func (store *MessageStore) MergeLIDs(ctx context.Context, pairs map[string]types.JID) error {
	var todo []string
	for lid := range pairs {
		if _, done := store.mergedLIDs.Load(lid); !done {
			todo = append(todo, lid)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, lid := range todo {
		if err := mergeLID(ctx, tx, lid, pairs[lid].String()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, lid := range todo {
		store.mergedLIDs.Store(lid, struct{}{})
	}
	return nil
}

// learnedLIDs returns the lid->pn pairs a live message proves: each pair is a
// JID as the stanza gave it and its canonical form, and a raw @lid that
// canonicalized to a phone number is a mapping.
func learnedLIDs(pairs ...[2]types.JID) map[string]types.JID {
	out := map[string]types.JID{}
	for _, p := range pairs {
		raw, canonical := p[0].ToNonAD(), p[1].ToNonAD()
		if raw.Server == types.HiddenUserServer && canonical.Server == types.DefaultUserServer {
			out[raw.String()] = canonical
		}
	}
	return out
}

// messageLIDs lists the lid->pn pairs a live message proves: its sender, the
// alt spelling the stanza carries, and its chat.
func messageLIDs(info types.MessageSource, chat types.JID, sender string) map[string]types.JID {
	canonicalSender, _ := types.ParseJID(sender)
	return learnedLIDs(
		[2]types.JID{info.Sender, canonicalSender},
		[2]types.JID{info.SenderAlt, info.Sender},
		[2]types.JID{info.Chat, chat},
	)
}
