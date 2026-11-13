package main

// Edits, deletes and reactions in messages.db.
//
// WhatsApp lets a sender edit a text message for 15 minutes, delete it "for
// everyone" (a revoke), and lets anyone react to it. The store mirrors all
// three: an edit rewrites content and stamps edited_at; a revoke leaves a
// tombstone (empty content, deleted_at, media pointers cleared) so the row is
// not mistaken for one that never existed; reactions live in their own table,
// one row per (message, reactor), a removed one kept with a blank emoji.
//
// The decision logic (who may edit or revoke, the edit window) is in pure
// functions so it can be tested without a WhatsApp client.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// editWindow is how long after sending WhatsApp still allows an edit.
const editWindow = 15 * time.Minute

// deleteWindow is how long after sending WhatsApp still honours a "delete for
// everyone" (2 days 12 hours). Recipients ignore a later revoke, so sending
// one would only erase the message from this store.
const deleteWindow = 60 * time.Hour

const reactionsSchema = `
	CREATE TABLE IF NOT EXISTS reactions (
		message_id TEXT NOT NULL,
		chat_jid   TEXT NOT NULL,
		sender     TEXT NOT NULL,
		emoji      TEXT NOT NULL,
		timestamp  TIMESTAMP,
		PRIMARY KEY (message_id, chat_jid, sender)
	)`

// migrateMutations brings an older messages.db up to the current schema:
// the edited_at/deleted_at columns and the reactions table. Idempotent; runs
// on every startup. Fresh stores already have the columns from their CREATE
// TABLE in NewMessageStore, so ALTER only fires for the ones that are missing.
func migrateMutations(db *sql.DB) error {
	rows, err := db.Query("SELECT name FROM pragma_table_info('messages')")
	if err != nil {
		return fmt.Errorf("reading messages schema: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning messages schema: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, col := range []string{"edited_at", "deleted_at"} {
		if have[col] {
			continue
		}
		if _, err := db.Exec("ALTER TABLE messages ADD COLUMN " + col + " TIMESTAMP"); err != nil {
			return fmt.Errorf("adding messages.%s: %w", col, err)
		}
	}
	if _, err := db.Exec(reactionsSchema); err != nil {
		return fmt.Errorf("creating reactions table: %w", err)
	}
	return nil
}

// adminCheck reports whether a revoker is currently an admin of the group the
// revoke arrived in. nil means "nobody is" (history sync, tests).
type adminCheck func() bool

// mayRevoke reports whether a revoke from revoker may tombstone a message
// stored under storedSender. The author always can. In a group an admin may
// also delete another participant's message, but the stanza is end-to-end
// encrypted and does not prove the revoker is one, so that case is confirmed
// through isAdmin — otherwise any member could erase anyone's messages from
// the store. isAdmin is only consulted for a non-author revoke in a group.
func mayRevoke(chatJID, revoker, storedSender string, isAdmin adminCheck) bool {
	if revoker == "" {
		return false
	}
	if revoker == storedSender {
		return true
	}
	return strings.HasSuffix(chatJID, "@"+types.GroupServer) && isAdmin != nil && isAdmin()
}

// withinEditWindow reports whether a message sent at sentAt can still be edited.
func withinEditWindow(sentAt, now time.Time) bool {
	return now.Sub(sentAt) <= editWindow
}

// withinDeleteWindow reports whether a message sent at sentAt can still be
// deleted for everyone.
func withinDeleteWindow(sentAt, now time.Time) bool {
	return now.Sub(sentAt) <= deleteWindow
}

// ApplyEdit rewrites a stored message's text. Only the author can edit, and
// never to nothing: it is a no-op (false, nil) when the message is unknown,
// already deleted, or the editor is not its stored sender.
func (store *MessageStore) ApplyEdit(chatJID, id, editor, newText string, at time.Time) (bool, error) {
	if newText == "" || editor == "" {
		return false, nil
	}
	res, err := store.db.Exec(
		`UPDATE messages SET content = ?, edited_at = ?
		 WHERE id = ? AND chat_jid = ? AND sender = ? AND deleted_at IS NULL`,
		newText, at, id, chatJID, editor,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ApplyRevoke turns a stored message into a tombstone and drops its reactions.
// media_type and filename stay so the row is not mistaken for an empty one.
// No-op (false, nil) when the message is unknown, already deleted, or the
// revoker is not allowed to delete it.
func (store *MessageStore) ApplyRevoke(chatJID, id, revoker string, isAdmin adminCheck, at time.Time) (bool, error) {
	// Decide before opening the transaction: isAdmin may be a network round
	// trip, and a transaction held across it would block other writers.
	var sender string
	err := store.db.QueryRow(
		"SELECT sender FROM messages WHERE id = ? AND chat_jid = ? AND deleted_at IS NULL", id, chatJID,
	).Scan(&sender)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !mayRevoke(chatJID, revoker, sender, isAdmin) {
		return false, nil
	}

	tx, err := store.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`UPDATE messages SET content = '', deleted_at = ?, url = NULL, media_key = NULL,
			file_sha256 = NULL, file_enc_sha256 = NULL, file_length = NULL
		 WHERE id = ? AND chat_jid = ? AND deleted_at IS NULL`, at, id, chatJID,
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.Exec("DELETE FROM reactions WHERE message_id = ? AND chat_jid = ?", id, chatJID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	removeCachedMedia(chatJID, id)
	return true, nil
}

// SetReaction records one reactor's current reaction to a message. It is a
// no-op when the message is unknown or already deleted for everyone, and when
// the row already holds a newer reaction: history sync can deliver a snapshot
// older than what arrived live. For the same reason a removal (empty emoji)
// keeps the row with a blank emoji instead of deleting it — without that
// marker a stale snapshot would bring the removed reaction back. Readers skip
// blank rows. The timestamp column holds TEXT with mixed offsets, so the
// comparison goes through julianday(); an unparseable value lets the write in.
func (store *MessageStore) SetReaction(messageID, chatJID, sender, emoji string, at time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO reactions (message_id, chat_jid, sender, emoji, timestamp)
		 SELECT ?, ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM messages WHERE id = ? AND chat_jid = ? AND deleted_at IS NULL)
		 ON CONFLICT(message_id, chat_jid, sender) DO UPDATE SET
			emoji = excluded.emoji, timestamp = excluded.timestamp
		 WHERE COALESCE(julianday(excluded.timestamp) >= julianday(reactions.timestamp), 1)`,
		messageID, chatJID, sender, emoji, at, messageID, chatJID,
	)
	return err
}

// StoredMessage is the slice of a messages row the REST mutation handlers
// need to decide whether an action is allowed.
type StoredMessage struct {
	Sender    string
	Content   string
	MediaType string
	IsFromMe  bool
	Time      time.Time
	Deleted   bool
}

// GetStoredMessage looks up a message by id and chat key; sql.ErrNoRows if absent.
func (store *MessageStore) GetStoredMessage(ctx context.Context, id, chatJID string) (StoredMessage, error) {
	var sender, content, mediaType sql.NullString
	var ts, deletedAt sql.NullTime
	var fromMe sql.NullBool
	err := store.db.QueryRowContext(ctx,
		`SELECT sender, content, media_type, is_from_me, timestamp, deleted_at
		 FROM messages WHERE id = ? AND chat_jid = ?`, id, chatJID,
	).Scan(&sender, &content, &mediaType, &fromMe, &ts, &deletedAt)
	return StoredMessage{
		Sender: sender.String, Content: content.String, MediaType: mediaType.String,
		IsFromMe: fromMe.Bool, Time: ts.Time, Deleted: deletedAt.Valid,
	}, err
}
