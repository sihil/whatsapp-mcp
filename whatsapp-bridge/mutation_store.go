package main

// Store upkeep that the edit/delete/reaction tools depend on: recording the
// bridge's own sends (so they have a row to edit or delete) and keeping the
// reactions table in step with the startup JID canonicalization.

import (
	"context"
	"database/sql"
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

// hasReactionsTable reports whether the store has a reactions table yet (a
// store last written by an older bridge, or a test schema, may not).
func hasReactionsTable(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'reactions'").Scan(&n)
	return n > 0, err
}

// moveColumnValue rewrites reactions.<column> from oldVal to newVal. A row
// already present under newVal wins, like the message rows it belongs to.
// column is one of the two constants below, never caller input.
func moveColumnValue(ctx context.Context, tx *sql.Tx, column, oldVal, newVal string) error {
	if _, err := tx.ExecContext(ctx,
		"UPDATE OR IGNORE reactions SET "+column+" = ? WHERE "+column+" = ?", newVal, oldVal); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM reactions WHERE "+column+" = ?", oldVal)
	return err
}

// moveReactionsChatKey follows a chat-key rename made by
// canonicalizeMessageStore, so reactions stay attached to their messages.
func moveReactionsChatKey(ctx context.Context, tx *sql.Tx, oldKey, newKey string) error {
	if ok, err := hasReactionsTable(ctx, tx); err != nil || !ok {
		return err
	}
	return moveColumnValue(ctx, tx, "chat_jid", oldKey, newKey)
}

// canonicalizeReactionSenders applies the same sender rule as messages.sender
// to reactors: no device part, modern PN server, PN instead of @lid once the
// mapping is known.
func canonicalizeReactionSenders(ctx context.Context, tx *sql.Tx, lids lidMapper) error {
	if ok, err := hasReactionsTable(ctx, tx); err != nil || !ok {
		return err
	}
	senders, err := collectColumn(tx, `SELECT DISTINCT sender FROM reactions`)
	if err != nil {
		return err
	}
	for _, stored := range senders {
		jid, err := types.ParseJID(stored)
		if err != nil || jid.User == "" {
			continue
		}
		canonical := normalizeLegacyServer(jid.ToNonAD())
		if canonical.Server == types.HiddenUserServer {
			if pn, err := lids.GetPNForLID(ctx, canonical); err == nil && !pn.IsEmpty() {
				canonical = pn.ToNonAD()
			}
		}
		if canonical.String() == stored {
			continue
		}
		if err := moveColumnValue(ctx, tx, "sender", stored, canonical.String()); err != nil {
			return err
		}
	}
	return nil
}
