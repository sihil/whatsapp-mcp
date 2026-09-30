package main

// Store upkeep that the edit/delete/reaction tools depend on: recording the
// bridge's own sends (so they have a row to edit or delete) and keeping the
// reactions table in step with the startup JID canonicalization.

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// recordSentMessage stores a message the bridge just sent. The sending device
// gets no echo of its own message, so without this a message sent through
// /api/send has no row and can be neither edited nor deleted afterwards.
func recordSentMessage(ctx context.Context, lids lidMapper, store *MessageStore, chat, ownJID types.JID,
	id string, at time.Time, msg *waE2E.Message) error {
	chatJID := canonicalChatJID(ctx, lids, chat.ToNonAD(), types.EmptyJID).String()
	content := extractTextContent(msg)
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg)
	if content == "" && mediaType == "" {
		return nil
	}
	// messages.chat_jid references chats(jid); StoreChat keeps a known name.
	if err := store.StoreChat(chatJID, "", at); err != nil {
		return err
	}
	return store.StoreMessage(id, chatJID, ownJID.ToNonAD().String(), content, at, true,
		mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength)
}

// AdoptSender rewrites one message's stored sender to canonical when it is
// currently one of aliases — other spellings of the same account. A row
// written while the PN<->LID mapping was unknown keeps its "@lid" sender until
// the next startup canonicalization; an edit or revoke arriving in between
// names the author by PN and would not match it.
func (store *MessageStore) AdoptSender(chatJID, id, canonical string, aliases []string) error {
	if len(aliases) == 0 {
		return nil
	}
	args := []any{canonical, id, chatJID}
	for _, alias := range aliases {
		args = append(args, alias)
	}
	_, err := store.db.Exec(
		"UPDATE messages SET sender = ? WHERE id = ? AND chat_jid = ? AND sender IN (?"+
			strings.Repeat(", ?", len(aliases)-1)+")", args...)
	return err
}

// moveColumnValue rewrites reactions.<column> from oldVal to newVal. When the
// same reactor has a row on the same message under both, the newer one wins,
// as in SetReaction: a row under newVal that is older than its counterpart is
// dropped first, and the UPDATE OR IGNORE then leaves the older counterparts
// behind for the DELETE. column is "chat_jid" or "sender", never caller input.
func moveColumnValue(ctx context.Context, tx *sql.Tx, column, oldVal, newVal string) error {
	other := "sender"
	if column == "sender" {
		other = "chat_jid"
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM reactions WHERE "+column+" = ? AND EXISTS (SELECT 1 FROM reactions o WHERE o."+column+" = ?"+
			" AND o.message_id = reactions.message_id AND o."+other+" = reactions."+other+
			" AND julianday(o.timestamp) > julianday(reactions.timestamp))", newVal, oldVal); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE OR IGNORE reactions SET "+column+" = ? WHERE "+column+" = ?", newVal, oldVal); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM reactions WHERE "+column+" = ?", oldVal)
	return err
}

// canonicalizeReactionSenders applies the same sender rule as messages.sender
// to reactors: no device part, modern PN server, PN instead of @lid once the
// mapping is known.
func canonicalizeReactionSenders(ctx context.Context, tx *sql.Tx, lids lidMapper) error {
	senders, err := collectColumn(tx, `SELECT DISTINCT sender FROM reactions`)
	if err != nil {
		return err
	}
	for _, stored := range senders {
		canonical, ok := canonicalStoredSender(ctx, lids, stored)
		if !ok {
			continue
		}
		if err := moveColumnValue(ctx, tx, "sender", stored, canonical); err != nil {
			return err
		}
	}
	return nil
}
