package main

// Group membership changes: who joined, left, or was promoted or demoted, by
// whom and when. They arrive live as events.GroupInfo and in history sync as
// participant stub messages, which carry no content and so would otherwise be
// dropped like any other empty message.

import (
	"context"
	"database/sql"
	"time"

	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// The key leaves out actor and reason: a live event and the history copy of
// the same change agree on who, what and when, but not always on the detail.
const groupEventsSchema = `
	CREATE TABLE IF NOT EXISTS group_events (
		chat_jid    TEXT NOT NULL,
		kind        TEXT NOT NULL,
		participant TEXT NOT NULL,
		actor       TEXT NOT NULL DEFAULT '',
		reason      TEXT NOT NULL DEFAULT '',
		timestamp   TIMESTAMP NOT NULL,
		PRIMARY KEY (chat_jid, kind, participant, timestamp)
	)`

// A copy with less detail never erases what an earlier one recorded.
const upsertGroupEvent = `
	INSERT INTO group_events (chat_jid, kind, participant, actor, reason, timestamp)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(chat_jid, kind, participant, timestamp) DO UPDATE SET
		actor = CASE WHEN group_events.actor = '' THEN excluded.actor ELSE group_events.actor END,
		reason = CASE WHEN group_events.reason = '' THEN excluded.reason ELSE group_events.reason END`

type groupEvent struct {
	chat        string
	kind        string // join, leave, promote, demote
	participant string
	actor       string // "" when unknown
	reason      string // join: added, invite, request, community; leave: left, removed
	at          time.Time
}

// groupEventTime is whole seconds in UTC, the precision history sync has, so
// the live and history copies of a change share a key.
func groupEventTime(at time.Time) time.Time {
	return at.UTC().Truncate(time.Second)
}

func groupEventTimestamp(at time.Time) string {
	return groupEventTime(at).Format("2006-01-02 15:04:05-07:00")
}

func groupEventsFromInfo(ctx context.Context, lids lidMapper, info *events.GroupInfo) []groupEvent {
	chat := info.JID.String()
	at := groupEventTime(info.Timestamp)
	actor := ""
	if info.SenderPN != nil && !info.SenderPN.IsEmpty() {
		actor = canonicalContactJID(ctx, lids, *info.SenderPN).String()
	} else if info.Sender != nil && !info.Sender.IsEmpty() {
		actor = canonicalContactJID(ctx, lids, *info.Sender).String()
	}

	var out []groupEvent
	add := func(kind string, jids []types.JID, reasonFor func(participant string) string) {
		for _, jid := range jids {
			participant := canonicalContactJID(ctx, lids, jid).String()
			out = append(out, groupEvent{chat, kind, participant, actor, reasonFor(participant), at})
		}
	}
	add("join", info.Join, func(participant string) string {
		switch {
		case info.JoinReason == "invite":
			return "invite"
		case actor != "" && actor != participant:
			return "added"
		}
		return ""
	})
	add("leave", info.Leave, func(participant string) string {
		switch {
		case actor == participant:
			return "left"
		case actor != "":
			return "removed"
		}
		return ""
	})
	none := func(string) string { return "" }
	add("promote", info.Promote, none)
	add("demote", info.Demote, none)
	return out
}

var historyGroupStubs = map[waWeb.WebMessageInfo_StubType][2]string{
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD:                           {"join", "added"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE:                        {"join", "invite"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD_REQUEST_JOIN:              {"join", "request"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_ACCEPT:                        {"join", "request"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_LINKED_GROUP_JOIN:             {"join", "community"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_JOINED_GROUP_AND_PARENT_GROUP: {"join", "community"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE:                        {"leave", "removed"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE:                         {"leave", "left"},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_PROMOTE:                       {"promote", ""},
	waWeb.WebMessageInfo_GROUP_PARTICIPANT_DEMOTE:                        {"demote", ""},
}

// groupEventsFromHistory reports whether a history-sync message is a
// membership stub, and the changes it records. The stub's sender is the
// actor; its parameters name the participants.
func groupEventsFromHistory(ctx context.Context, lids lidMapper, wmi *waWeb.WebMessageInfo,
	chat, ownJID types.JID, pnMap map[string]types.JID) ([]groupEvent, bool) {
	stub, ok := historyGroupStubs[wmi.GetMessageStubType()]
	if !ok {
		return nil, false
	}
	kind, reason := stub[0], stub[1]
	actor := ""
	fromMe := wmi.GetKey().GetFromMe()
	if p := wmi.GetKey().GetParticipant(); p != "" || fromMe {
		actor = canonicalHistorySenderJID(ctx, lids, p, fromMe, chat, ownJID, pnMap)
	} else if p := wmi.GetParticipant(); p != "" {
		actor = canonicalHistorySenderJID(ctx, lids, p, false, chat, ownJID, pnMap)
	}
	at := groupEventTime(time.Unix(int64(wmi.GetMessageTimestamp()), 0))

	var out []groupEvent
	for _, param := range wmi.GetMessageStubParameters() {
		participant := canonicalHistorySenderJID(ctx, lids, param, false, chat, ownJID, pnMap)
		out = append(out, groupEvent{chat.String(), kind, participant, actor, reason, at})
	}
	return out, true
}

func (store *MessageStore) StoreGroupEvents(evts []groupEvent) error {
	for _, e := range evts {
		if _, err := store.db.Exec(upsertGroupEvent,
			e.chat, e.kind, e.participant, e.actor, e.reason, groupEventTimestamp(e.at)); err != nil {
			return err
		}
	}
	return nil
}

// moveGroupEventIdentity rewrites one person's JID wherever group_events
// names them. A row already stored under the new JID is the same change and
// absorbs the moved row's detail.
func moveGroupEventIdentity(ctx context.Context, tx *sql.Tx, oldJID, newJID string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO group_events (chat_jid, kind, participant, actor, reason, timestamp)
		SELECT chat_jid, kind, ?, actor, reason, timestamp FROM group_events WHERE participant = ?
		ON CONFLICT(chat_jid, kind, participant, timestamp) DO UPDATE SET
			actor = CASE WHEN group_events.actor = '' THEN excluded.actor ELSE group_events.actor END,
			reason = CASE WHEN group_events.reason = '' THEN excluded.reason ELSE group_events.reason END`,
		newJID, oldJID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_events WHERE participant = ?`, oldJID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE group_events SET actor = ? WHERE actor = ?`, newJID, oldJID)
	return err
}

// canonicalizeGroupEvents applies the messages.sender rule to participants
// and actors.
func canonicalizeGroupEvents(ctx context.Context, tx *sql.Tx, lids lidMapper) error {
	stored, err := collectColumn(tx, `
		SELECT participant FROM group_events UNION SELECT actor FROM group_events WHERE actor <> ''`)
	if err != nil {
		return err
	}
	for _, jid := range stored {
		canonical, ok := canonicalStoredSender(ctx, lids, jid)
		if !ok {
			continue
		}
		if err := moveGroupEventIdentity(ctx, tx, jid, canonical); err != nil {
			return err
		}
	}
	return nil
}
