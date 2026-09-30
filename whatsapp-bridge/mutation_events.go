package main

// Incoming reactions, edits and revokes: classifying the protocol shapes and
// applying them to the store. whatsmeow delivers all three as *events.Message
// (already unwrapped), and history sync carries them on WebMessageInfo.

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type mutationKind int

const (
	mutationNone mutationKind = iota
	mutationReaction
	mutationEncReaction // emoji is only known after client.DecryptReaction
	mutationEdit
	mutationRevoke
)

// mutation is a classified edit/revoke/reaction stanza. at is the zero time
// when the stanza carries no timestamp of its own.
type mutation struct {
	kind     mutationKind
	targetID string
	emoji    string // reaction: "" means removed
	text     string // edit: the new text
	at       time.Time
}

// classifyMutation inspects an unwrapped message. Some clients wrap an edit in
// EditedMessage; UnwrapRaw normally strips that (leaving the ProtocolMessage
// in Message with IsEdit set), but an unstripped wrapper is handled too.
func classifyMutation(m *waE2E.Message) mutation {
	if inner := m.GetEditedMessage().GetMessage(); inner != nil {
		m = inner
	}
	if r := m.GetReactionMessage(); r != nil {
		mu := mutation{kind: mutationReaction, targetID: r.GetKey().GetID(), emoji: r.GetText()}
		if ms := r.GetSenderTimestampMS(); ms != 0 {
			mu.at = time.UnixMilli(ms)
		}
		return mu
	}
	if enc := m.GetEncReactionMessage(); enc != nil {
		return mutation{kind: mutationEncReaction, targetID: enc.GetTargetMessageKey().GetID()}
	}
	pm := m.GetProtocolMessage()
	if pm == nil {
		return mutation{}
	}
	mu := mutation{targetID: pm.GetKey().GetID()}
	if ms := pm.GetTimestampMS(); ms != 0 {
		mu.at = time.UnixMilli(ms)
	}
	switch pm.GetType() {
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		mu.kind = mutationEdit
		mu.text = extractTextContent(pm.GetEditedMessage())
	case waE2E.ProtocolMessage_REVOKE:
		mu.kind = mutationRevoke
	default:
		return mutation{}
	}
	return mu
}

// handleMutationEvent applies a live reaction, edit or revoke and reports
// whether the event was one (so the caller must not store it as a message).
// chatJID and sender are the canonical strings handleMessage computed.
func handleMutationEvent(client *whatsmeow.Client, store *MessageStore, msg *events.Message, chatJID, sender string, logger waLog.Logger) bool {
	mu := classifyMutation(msg.Message)
	if mu.kind == mutationNone {
		return false
	}
	if mu.targetID == "" {
		logger.Warnf("Ignoring mutation with no target message from %s", sender)
		return true
	}
	at := mu.at
	if at.IsZero() {
		at = msg.Info.Timestamp
	}
	if mu.kind == mutationEncReaction {
		dec, err := client.DecryptReaction(context.Background(), msg)
		if err != nil {
			logger.Warnf("Failed to decrypt reaction from %s: %v", sender, err)
			return true
		}
		mu.kind, mu.emoji = mutationReaction, dec.GetText()
		if ms := dec.GetSenderTimestampMS(); ms != 0 {
			at = time.UnixMilli(ms)
		}
	}
	if mu.kind != mutationReaction && !msg.Info.IsFromMe {
		adoptSender(context.Background(), client.Store.LIDs, store, chatJID, mu.targetID, sender, logger,
			msg.Info.Sender, msg.Info.SenderAlt)
	}
	applyMutation(store, mu, chatJID, sender, groupAdminCheck(client, msg.Info.Chat, msg.Info.Sender, logger), at, logger)
	return true
}

// adoptSender brings the target row's sender up to date before an edit or
// revoke is matched against it (see AdoptSender). canonical is the author as
// the event names them; spellings are the other forms the event carries, and
// the LID store supplies the LID behind a PN author.
func adoptSender(ctx context.Context, lids lidMapper, store *MessageStore, chatJID, id, canonical string,
	logger waLog.Logger, spellings ...types.JID) {
	if pn, err := types.ParseJID(canonical); err == nil && pn.Server == types.DefaultUserServer {
		if lid, err := lids.GetLIDForPN(ctx, pn); err == nil {
			spellings = append(spellings, lid)
		}
	}
	var aliases []string
	for _, jid := range spellings {
		if s := jid.ToNonAD().String(); !jid.IsEmpty() && s != canonical {
			aliases = append(aliases, s)
		}
	}
	if err := store.AdoptSender(chatJID, id, canonical, aliases); err != nil {
		logger.Warnf("Failed to update the sender of %s: %v", id, err)
	}
}

// groupAdminCheck builds the adminCheck for a live revoke: it asks WhatsApp
// for the group's current roster, so it costs a round trip — mayRevoke only
// calls it for the rare revoke of someone else's message.
func groupAdminCheck(client *whatsmeow.Client, chat, revoker types.JID, logger waLog.Logger) adminCheck {
	return func() bool {
		info, err := client.GetGroupInfo(context.Background(), chat)
		if err != nil {
			logger.Warnf("Could not confirm %s is an admin of %s: %v", revoker, chat, err)
			return false
		}
		return isAdminIn(info.Participants, revoker)
	}
}

// isAdminIn reports whether jid is an admin among participants. A participant
// may be listed under its phone-number JID or its LID, so every spelling the
// roster carries is compared (user part only; device suffixes differ).
func isAdminIn(participants []types.GroupParticipant, jid types.JID) bool {
	for _, p := range participants {
		if !p.IsAdmin && !p.IsSuperAdmin {
			continue
		}
		for _, spelling := range []types.JID{p.JID, p.PhoneNumber, p.LID} {
			if !spelling.IsEmpty() && spelling.User == jid.User && spelling.Server == jid.Server {
				return true
			}
		}
	}
	return false
}

// applyMutation writes one classified mutation to the store.
func applyMutation(store *MessageStore, mu mutation, chatJID, sender string, isAdmin adminCheck, at time.Time, logger waLog.Logger) {
	var applied bool
	var err error
	switch mu.kind {
	case mutationReaction:
		err = store.SetReaction(mu.targetID, chatJID, sender, mu.emoji, at)
		applied = err == nil
	case mutationEdit:
		applied, err = store.ApplyEdit(chatJID, mu.targetID, sender, mu.text, at)
	case mutationRevoke:
		applied, err = store.ApplyRevoke(chatJID, mu.targetID, sender, isAdmin, at)
	}
	if err != nil {
		logger.Warnf("Failed to apply mutation %d to %s: %v", mu.kind, mu.targetID, err)
	} else if !applied {
		logger.Debugf("Mutation %d for %s not applied (unknown target or not permitted)", mu.kind, mu.targetID)
	}
}

// applyHistoryReactions records the reactions carried by a history-sync
// message. Call it after the message itself is stored: SetReaction ignores a
// reaction whose target has no row, so a message the bridge does not keep
// (poll, sticker, ...) leaves no orphan reactions behind.
func applyHistoryReactions(ctx context.Context, lids lidMapper, store *MessageStore, wmi *waWeb.WebMessageInfo,
	chat, ownJID types.JID, pnMap map[string]types.JID, logger waLog.Logger) {
	id := wmi.GetKey().GetID()
	if id == "" {
		return
	}
	chatJID := chat.String()
	msgTime := time.Unix(int64(wmi.GetMessageTimestamp()), 0)
	for _, r := range wmi.GetReactions() {
		reactor := canonicalHistorySenderJID(ctx, lids, r.GetKey().GetParticipant(), r.GetKey().GetFromMe(), chat, ownJID, pnMap)
		at := msgTime
		if ms := r.GetSenderTimestampMS(); ms != 0 {
			at = time.UnixMilli(ms)
		}
		if err := store.SetReaction(id, chatJID, reactor, r.GetText(), at); err != nil {
			logger.Warnf("Failed to store history reaction on %s: %v", id, err)
		}
	}
}

// applyHistoryRevoke reports whether a history-sync message is a REVOKE stub —
// a deleted message, which has no content and must not be stored — and
// tombstones the row it refers to.
func applyHistoryRevoke(ctx context.Context, lids lidMapper, store *MessageStore, wmi *waWeb.WebMessageInfo,
	chat, ownJID types.JID, pnMap map[string]types.JID, logger waLog.Logger) bool {
	id := wmi.GetKey().GetID()
	if id == "" || wmi.GetMessageStubType() != waWeb.WebMessageInfo_REVOKE {
		return false
	}
	chatJID := chat.String()
	participant := wmi.GetKey().GetParticipant()
	if participant == "" {
		participant = wmi.GetParticipant()
	}
	fromMe := wmi.GetKey().GetFromMe()
	sender := canonicalHistorySenderJID(ctx, lids, participant, fromMe, chat, ownJID, pnMap)
	if raw, err := types.ParseJID(participant); err == nil && !fromMe {
		adoptSender(ctx, lids, store, chatJID, id, sender, logger, raw)
	}
	if _, err := store.ApplyRevoke(chatJID, id, sender, nil, time.Unix(int64(wmi.GetMessageTimestamp()), 0)); err != nil {
		logger.Warnf("Failed to apply history revoke of %s: %v", id, err)
	}
	return true
}
