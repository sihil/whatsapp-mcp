package main

// Identity canonicalization for messages.db.
//
// WhatsApp addresses chats and senders under two namespaces: phone numbers
// ("@s.whatsapp.net") and LIDs ("@lid"). The store's invariant, established
// by the write paths below and by canonicalizeMessageStore for legacy rows,
// is: sender and chat_jid are always full "user@server" JID strings, using
// the phone-number form whenever the PN<->LID mapping is known, the "@lid"
// form as an honest fallback, and the own PN JID for is_from_me rows.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
)

// lidMapper is the subset of whatsmeow's LID store used to translate between
// the phone-number and LID namespaces.
type lidMapper interface {
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
	GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error)
}

// legacyPNServer is the pre-multidevice spelling of the phone-number server
// that still appears in history-sync payloads and old rows.
const legacyPNServer = "c.us"

func normalizeLegacyServer(jid types.JID) types.JID {
	if jid.Server == legacyPNServer {
		jid.Server = types.DefaultUserServer
	}
	return jid
}

// canonicalSenderJID returns the canonical sender string for a live message:
// the own PN JID for own messages, otherwise the sender PN-ified via the
// stanza's alt JID or the LID store when possible.
func canonicalSenderJID(ctx context.Context, lids lidMapper, sender, senderAlt, ownJID types.JID, isFromMe bool) string {
	if isFromMe && !ownJID.IsEmpty() {
		return ownJID.ToNonAD().String()
	}
	return canonicalChatJID(ctx, lids, sender.ToNonAD(), senderAlt).String()
}

// canonicalChatJID returns the canonical chat key for a live message. alt is
// the peer's JID in the other namespace: SenderAlt for incoming messages,
// RecipientAlt for own-outgoing DMs.
func canonicalChatJID(ctx context.Context, lids lidMapper, chat, alt types.JID) types.JID {
	if chat.Server != types.HiddenUserServer {
		return chat
	}
	if !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	if pn, err := lids.GetPNForLID(ctx, chat); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return chat
}

// historyPNMap extracts the PN<->LID mapping table carried by a history-sync
// payload, keyed by the LID JID string. whatsmeow ingests these mappings into
// the LID store asynchronously, racing the event dispatch, so the payload's
// own table must be consulted before the store.
func historyPNMap(data *waHistorySync.HistorySync) map[string]types.JID {
	m := make(map[string]types.JID)
	for _, mapping := range data.GetPhoneNumberToLidMappings() {
		pn, err := types.ParseJID(mapping.GetPnJID())
		if err != nil || pn.User == "" {
			continue
		}
		lid, err := types.ParseJID(mapping.GetLidJID())
		if err != nil || lid.User == "" {
			continue
		}
		m[lid.ToNonAD().String()] = normalizeLegacyServer(pn.ToNonAD())
	}
	return m
}

// canonicalHistoryChatJID returns the canonical chat key for a history-sync
// conversation: the conversation's own PN JID when present, then the payload
// mapping table, then the LID store.
func canonicalHistoryChatJID(ctx context.Context, lids lidMapper, chat types.JID, convPnJID string, pnMap map[string]types.JID) types.JID {
	chat = normalizeLegacyServer(chat)
	if chat.Server != types.HiddenUserServer {
		return chat
	}
	if convPnJID != "" {
		if pn, err := types.ParseJID(convPnJID); err == nil && pn.User != "" {
			return normalizeLegacyServer(pn.ToNonAD())
		}
	}
	if pn, ok := pnMap[chat.String()]; ok {
		return pn
	}
	if pn, err := lids.GetPNForLID(ctx, chat); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return chat
}

// canonicalHistorySenderJID returns the canonical sender string for a
// history-sync row. participant is Message.Key.Participant (may be empty for
// DM rows), chat the already-canonicalized chat key.
func canonicalHistorySenderJID(ctx context.Context, lids lidMapper, participant string, isFromMe bool, chat, ownJID types.JID, pnMap map[string]types.JID) string {
	if isFromMe && !ownJID.IsEmpty() {
		return ownJID.ToNonAD().String()
	}
	if participant != "" {
		jid, err := types.ParseJID(participant)
		if err == nil && jid.User != "" {
			jid = normalizeLegacyServer(jid.ToNonAD())
			if jid.Server == types.HiddenUserServer {
				if pn, ok := pnMap[jid.String()]; ok {
					return pn.String()
				}
				if pn, lidErr := lids.GetPNForLID(ctx, jid); lidErr == nil && !pn.IsEmpty() {
					return pn.ToNonAD().String()
				}
			}
			return jid.String()
		}
	}
	// DM rows carry no participant: the sender is the chat peer.
	return chat.String()
}

// resolveReplyParticipant works out the JID to put in a quoted reply's
// ContextInfo.Participant. The sender column holds a full canonical JID
// (PN-preferred; see canonicalizeMessageStore), with bare digits only as a
// legacy safety net. Group messages are LID-addressed on recipients' clients,
// so the LID form is preferred there when the mapping is known; WhatsApp's
// tap-to-jump keys primarily off StanzaID, so an imprecise participant
// degrades attribution display only.
func resolveReplyParticipant(ctx context.Context, lids lidMapper, storedSender string, isFromMe bool, chatJID, ownJID types.JID) types.JID {
	var participant types.JID
	switch {
	case isFromMe:
		participant = ownJID
	case strings.Contains(storedSender, "@"):
		if jid, err := types.ParseJID(storedSender); err == nil {
			participant = jid
		}
	}
	if participant.IsEmpty() {
		if chatJID.Server != types.GroupServer {
			participant = chatJID
		} else {
			participant = types.NewJID(storedSender, types.DefaultUserServer)
		}
	}
	if chatJID.Server == types.GroupServer && participant.Server == types.DefaultUserServer {
		if lid, err := lids.GetLIDForPN(ctx, participant); err == nil && !lid.IsEmpty() {
			participant = lid
		}
	}
	return participant
}

// canonicalizeMessageStore rewrites legacy sender and chat_jid formats in the
// message store to the canonical form described at the top of this file. It
// is idempotent and safe to run on every startup: re-runs pick up PN<->LID
// mappings the store has learned since the rows were written. Returns the
// number of rows changed.
func canonicalizeMessageStore(ctx context.Context, db *sql.DB, lids lidMapper, ownJID types.JID) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	changed := 0
	count := func(res sql.Result, err error) error {
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed += int(n)
		return nil
	}

	// Readers holding a stale chat key follow renames through this table
	// (the Python layer has no access to the LID store).
	if _, err := tx.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS jid_aliases (old_jid TEXT PRIMARY KEY, new_jid TEXT NOT NULL)`); err != nil {
		return 0, err
	}

	// mergeChatKey folds the chat row and messages under oldKey into newKey,
	// leaving an alias behind. The same message id can exist under both keys
	// (captured live under one namespace, history-synced under the other),
	// so moves are conflict-aware: the row already under newKey wins. The
	// timestamp column holds TEXT with mixed local offsets, so comparison
	// must go through datetime(), not raw MAX().
	mergeChatKey := func(oldKey, newKey string) error {
		if err := count(tx.ExecContext(ctx, `
			INSERT INTO jid_aliases (old_jid, new_jid) VALUES (?, ?)
			ON CONFLICT(old_jid) DO UPDATE SET new_jid = excluded.new_jid`,
			oldKey, newKey)); err != nil {
			return err
		}
		if err := count(tx.ExecContext(ctx, `
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
			return err
		}
		if err := count(tx.ExecContext(ctx, `UPDATE OR IGNORE messages SET chat_jid = ? WHERE chat_jid = ?`, newKey, oldKey)); err != nil {
			return err
		}
		if err := count(tx.ExecContext(ctx, `DELETE FROM messages WHERE chat_jid = ?`, oldKey)); err != nil {
			return err
		}
		return count(tx.ExecContext(ctx, `DELETE FROM chats WHERE jid = ?`, oldKey))
	}

	// 1a. Merge @lid-keyed DM chats into their PN key where the mapping is
	// known.
	lidChats, err := collectColumn(tx, `SELECT jid FROM chats WHERE jid LIKE '%@lid'`)
	if err != nil {
		return 0, err
	}
	for _, lidKey := range lidChats {
		lid, err := types.ParseJID(lidKey)
		if err != nil {
			continue
		}
		pn, err := lids.GetPNForLID(ctx, lid)
		if err != nil || pn.IsEmpty() {
			continue
		}
		if err := mergeChatKey(lidKey, pn.ToNonAD().String()); err != nil {
			return 0, err
		}
	}

	// 1b. Rename legacy @c.us chat keys to the modern PN server.
	legacyChats, err := collectColumn(tx, `SELECT jid FROM chats WHERE jid LIKE '%@c.us'`)
	if err != nil {
		return 0, err
	}
	for _, oldKey := range legacyChats {
		jid, err := types.ParseJID(oldKey)
		if err != nil {
			continue
		}
		if err := mergeChatKey(oldKey, normalizeLegacyServer(jid).String()); err != nil {
			return 0, err
		}
	}

	// 2. Own rows always carry the own PN JID.
	if !ownJID.IsEmpty() {
		own := ownJID.ToNonAD().String()
		if err := count(tx.ExecContext(ctx,
			`UPDATE messages SET sender = ? WHERE is_from_me = 1 AND sender <> ?`, own, own)); err != nil {
			return 0, err
		}
	}

	// 3. A bare-digit sender in a DM is the chat peer (verified to hold for
	// every such row): store the chat key itself.
	if err := count(tx.ExecContext(ctx, `
		UPDATE messages SET sender = chat_jid
		WHERE is_from_me = 0 AND sender NOT LIKE '%@%'
		AND (chat_jid LIKE '%@s.whatsapp.net' OR chat_jid LIKE '%@lid')`)); err != nil {
		return 0, err
	}

	// 4. Bare digits in a multi-party chat (group, broadcast, newsletter)
	// are LID digits when the LID store knows them as such; otherwise assume
	// the PN namespace (same best-effort guess the read path used to make,
	// frozen into data).
	const multiPartyChat = `(chat_jid LIKE '%@g.us' OR chat_jid LIKE '%@broadcast' OR chat_jid LIKE '%@newsletter')`
	groupBare, err := collectColumn(tx, `
		SELECT DISTINCT sender FROM messages
		WHERE is_from_me = 0 AND sender NOT LIKE '%@%' AND `+multiPartyChat)
	if err != nil {
		return 0, err
	}
	for _, digits := range groupBare {
		target := types.NewJID(digits, types.DefaultUserServer)
		if pn, err := lids.GetPNForLID(ctx, types.NewJID(digits, types.HiddenUserServer)); err == nil && !pn.IsEmpty() {
			target = pn.ToNonAD()
		}
		if err := count(tx.ExecContext(ctx,
			`UPDATE messages SET sender = ? WHERE sender = ? AND `+multiPartyChat, target.String(), digits)); err != nil {
			return 0, err
		}
	}

	// 5. Full-JID senders: strip device parts, normalize the legacy @c.us
	// server spelling, and PN-ify @lid where mapped.
	fullSenders, err := collectColumn(tx, `SELECT DISTINCT sender FROM messages WHERE sender LIKE '%@%'`)
	if err != nil {
		return 0, err
	}
	for _, stored := range fullSenders {
		jid, err := types.ParseJID(stored)
		if err != nil {
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
		if err := count(tx.ExecContext(ctx,
			`UPDATE messages SET sender = ? WHERE sender = ?`, canonical.String(), stored)); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

func collectColumn(tx *sql.Tx, query string) ([]string, error) {
	rows, err := tx.Query(query)
	if err != nil {
		return nil, fmt.Errorf("querying %q: %w", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
