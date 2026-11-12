package main

// Community structure: which community each group belongs to, which group is
// a community's announcements group, and the community's subgroups the
// account is not in. The read layer needs it offline, to expand an allowlist
// entry that covers a whole community and to list communities.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// parent_jid is ” for a group in no community, and for a community itself.
const groupLinksSchema = `
	CREATE TABLE IF NOT EXISTS group_links (
		group_jid       TEXT PRIMARY KEY,
		parent_jid      TEXT NOT NULL DEFAULT '',
		is_community    INTEGER NOT NULL DEFAULT 0,
		is_announcement INTEGER NOT NULL DEFAULT 0,
		name            TEXT NOT NULL DEFAULT '',
		is_member       INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_group_links_parent ON group_links(parent_jid);
	CREATE TABLE IF NOT EXISTS community_syncs (
		community_jid TEXT PRIMARY KEY,
		synced_at     TIMESTAMP NOT NULL
	)`

// subGroupsMaxAge is how long a community's subgroup list is trusted before
// the bridge asks again; live link events keep it current in between.
const subGroupsMaxAge = 24 * time.Hour

var errSubGroupsRateLimited = errors.New("WhatsApp rate-limited the subgroup lookups")

// upsertGroupLink takes a full snapshot of one group: its parent is
// authoritative, but an unknown name never blanks a known one.
const upsertGroupLink = `
	INSERT INTO group_links (group_jid, parent_jid, is_community, is_announcement, name)
	VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(group_jid) DO UPDATE SET
		parent_jid = excluded.parent_jid,
		is_community = excluded.is_community,
		is_announcement = excluded.is_announcement,
		name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE group_links.name END`

func jidOrEmpty(jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	return jid.String()
}

// PutGroupInfo records a group the account is in.
func (store *MessageStore) PutGroupInfo(info *types.GroupInfo) error {
	if _, err := store.db.Exec(upsertGroupLink, info.JID.String(), jidOrEmpty(info.LinkedParentJID),
		info.IsParent, info.IsDefaultSubGroup, info.Name); err != nil {
		return err
	}
	_, err := store.db.Exec(`UPDATE group_links SET is_member = 1 WHERE group_jid = ?`, info.JID.String())
	return err
}

// migrateGroupLinks adds is_member to a group_links created before it
// existed. Existing rows read as not joined until the next sync.
func migrateGroupLinks(db *sql.DB) error {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('group_links') WHERE name = 'is_member'`,
	).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := db.Exec(`ALTER TABLE group_links ADD COLUMN is_member INTEGER NOT NULL DEFAULT 0`)
	return err
}

// ApplyOwnMembership tracks the account joining or leaving a group between
// syncs, from the membership changes of a live event.
func (store *MessageStore) ApplyOwnMembership(evts []groupEvent, own string) error {
	for _, e := range evts {
		if own == "" || e.participant != own {
			continue
		}
		var err error
		switch e.kind {
		case "join":
			_, err = store.db.Exec(`
				INSERT INTO group_links (group_jid, is_member) VALUES (?, 1)
				ON CONFLICT(group_jid) DO UPDATE SET is_member = 1`, e.chat)
		case "leave":
			_, err = store.db.Exec(`UPDATE group_links SET is_member = 0 WHERE group_jid = ?`, e.chat)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// PutSubGroups records a community and the subgroups WhatsApp lists for it,
// whether or not the account is in them.
func (store *MessageStore) PutSubGroups(community types.JID, subs []*types.GroupLinkTarget) error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO group_links (group_jid, parent_jid, is_community) VALUES (?, '', 1)
		ON CONFLICT(group_jid) DO UPDATE SET parent_jid = '', is_community = 1`,
		community.String()); err != nil {
		return err
	}
	for _, sub := range subs {
		if _, err := tx.Exec(upsertGroupLink, sub.JID.String(), community.String(),
			false, sub.IsDefaultSubGroup, sub.Name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type groupLister interface {
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetSubGroups(ctx context.Context, community types.JID) ([]*types.GroupLinkTarget, error)
}

// syncGroupLinks records every joined group, then asks each community it
// found, by membership or as a group's parent, for its subgroups. Lookups are
// paced by pause, a community synced within subGroupsMaxAge of now is skipped,
// and a rate limit ends the pass early with errSubGroupsRateLimited: those
// left over are asked on a later pass. It reports the communities recorded
// and those still due.
func syncGroupLinks(ctx context.Context, lister groupLister, store *MessageStore, now time.Time, pause func()) (recorded, remaining int, err error) {
	joined, err := lister.GetJoinedGroups(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("listing joined groups: %w", err)
	}
	var due []types.JID
	seen := map[string]bool{}
	note := func(jid types.JID) {
		if jid.IsEmpty() || seen[jid.String()] {
			return
		}
		seen[jid.String()] = true
		if fresh, err := store.subGroupsFresh(jid.String(), now); err == nil && !fresh {
			due = append(due, jid)
		}
	}
	joinedJIDs := make([]any, 0, len(joined))
	for _, info := range joined {
		if err := store.PutGroupInfo(info); err != nil {
			return 0, 0, err
		}
		joinedJIDs = append(joinedJIDs, info.JID.String())
		if info.IsParent {
			note(info.JID)
		}
		note(info.LinkedParentJID)
	}
	// The joined list is complete, so anything missing from it was left.
	notJoined := `UPDATE group_links SET is_member = 0 WHERE is_member = 1`
	if len(joinedJIDs) > 0 {
		notJoined += ` AND group_jid NOT IN (?` + strings.Repeat(",?", len(joinedJIDs)-1) + `)`
	}
	if _, err := store.db.Exec(notJoined, joinedJIDs...); err != nil {
		return 0, 0, err
	}

	var failures []string
	for i, community := range due {
		if i > 0 {
			pause()
		}
		subs, err := lister.GetSubGroups(ctx, community)
		if errors.Is(err, whatsmeow.ErrIQRateOverLimit) {
			return recorded, len(due) - i, errSubGroupsRateLimited
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", community, err))
			continue
		}
		if err := store.PutSubGroups(community, subs); err != nil {
			return recorded, len(due) - i, err
		}
		if _, err := store.db.Exec(`
			INSERT INTO community_syncs (community_jid, synced_at) VALUES (?, ?)
			ON CONFLICT(community_jid) DO UPDATE SET synced_at = excluded.synced_at`,
			community.String(), now.UTC().Format(time.RFC3339)); err != nil {
			return recorded, len(due) - i, err
		}
		recorded++
	}
	if len(failures) > 0 {
		return recorded, 0, fmt.Errorf("listing subgroups: %s", strings.Join(failures, "; "))
	}
	return recorded, 0, nil
}

func (store *MessageStore) subGroupsFresh(community string, now time.Time) (bool, error) {
	var syncedAt string
	err := store.db.QueryRow(`SELECT synced_at FROM community_syncs WHERE community_jid = ?`, community).Scan(&syncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at, err := time.Parse(time.RFC3339, syncedAt)
	if err != nil {
		return false, nil
	}
	return now.Sub(at) < subGroupsMaxAge, nil
}

func (store *MessageStore) parentOf(group string) (string, error) {
	var parent string
	err := store.db.QueryRow(`SELECT parent_jid FROM group_links WHERE group_jid = ?`, group).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return parent, err
}

// linkSides works out which group a link change moves and which community it
// concerns. The event is about info.JID; the change names the other side. A
// sibling's community is info.JID's own, if known.
func (store *MessageStore) linkSides(info *events.GroupInfo, change *types.GroupLinkChange) (child, community string, err error) {
	switch change.Type {
	case types.GroupLinkChangeTypeSub:
		return change.Group.JID.String(), info.JID.String(), nil
	case types.GroupLinkChangeTypeParent:
		return info.JID.String(), change.Group.JID.String(), nil
	case types.GroupLinkChangeTypeSibling:
		parent, err := store.parentOf(info.JID.String())
		return change.Group.JID.String(), parent, err
	}
	return "", "", nil
}

var unlinkReasons = map[types.GroupUnlinkReason]string{
	types.GroupUnlinkReasonDefault: "unlinked",
	types.GroupUnlinkReasonDelete:  "community deleted",
}

// ApplyGroupLinkChanges updates group_links for a group being linked to or
// unlinked from a community, and returns the change as group events keyed by
// the community.
func (store *MessageStore) ApplyGroupLinkChanges(ctx context.Context, lids lidMapper, info *events.GroupInfo) ([]groupEvent, error) {
	actor := ""
	if info.SenderPN != nil && !info.SenderPN.IsEmpty() {
		actor = canonicalContactJID(ctx, lids, *info.SenderPN).String()
	} else if info.Sender != nil && !info.Sender.IsEmpty() {
		actor = canonicalContactJID(ctx, lids, *info.Sender).String()
	}
	at := groupEventTime(info.Timestamp)

	var out []groupEvent
	if change := info.Link; change != nil {
		child, community, err := store.linkSides(info, change)
		if err != nil {
			return nil, err
		}
		if child != "" && community != "" {
			if _, err := store.db.Exec(`
				INSERT INTO group_links (group_jid, parent_jid, is_announcement, name) VALUES (?, ?, ?, ?)
				ON CONFLICT(group_jid) DO UPDATE SET
					parent_jid = excluded.parent_jid,
					is_announcement = excluded.is_announcement,
					name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE group_links.name END`,
				child, community, change.Group.IsDefaultSubGroup && child == change.Group.JID.String(),
				nameIfChild(child, change)); err != nil {
				return nil, err
			}
			out = append(out, groupEvent{community, "link", child, actor, "", at})
		}
	}
	if change := info.Unlink; change != nil {
		child, community, err := store.linkSides(info, change)
		if err != nil {
			return nil, err
		}
		if child != "" && community != "" {
			if _, err := store.db.Exec(
				`UPDATE group_links SET parent_jid = '', is_announcement = 0 WHERE group_jid = ? AND parent_jid = ?`,
				child, community); err != nil {
				return nil, err
			}
			out = append(out, groupEvent{community, "unlink", child, actor, unlinkReasons[change.UnlinkReason], at})
		}
	}
	return out, nil
}

// The change carries the name of the group on the other side, which is the
// child only when the event is about the community or a sibling.
func nameIfChild(child string, change *types.GroupLinkChange) string {
	if change.Group.JID.String() == child {
		return change.Group.Name
	}
	return ""
}

type contactGetter interface {
	GetContact(ctx context.Context, jid types.JID) (types.ContactInfo, error)
}

// communityMember describes one participant of a community, PN-canonical and
// named from the address book where possible, plus the names learned.
func communityMember(ctx context.Context, lids lidMapper, contacts contactGetter, jid types.JID) (GroupMember, *ContactNames) {
	canonical := canonicalContactJID(ctx, lids, jid)
	member := GroupMember{JID: canonical.String(), PhoneNumber: canonical.User}
	if canonical.Server != types.DefaultUserServer {
		member.PhoneNumber = ""
	}
	contact, err := contacts.GetContact(ctx, canonical)
	if err != nil || !contact.Found {
		return member, nil
	}
	member.DisplayName = bestContactName(contact)
	names := contactNamesOf(canonical.String(), contact)
	return member, &names
}

// syncCommunities runs syncGroupLinks until no community is due, waiting out
// a rate limit between passes. One pass at a time: a reconnect while a pass
// is waiting does not start another.
func syncCommunities(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	if !store.syncingCommunities.CompareAndSwap(false, true) {
		return
	}
	defer store.syncingCommunities.Store(false)
	for attempt := 1; attempt <= 6; attempt++ {
		recorded, remaining, err := syncGroupLinks(context.Background(), client, store, time.Now(),
			func() { time.Sleep(2 * time.Second) })
		logger.Infof("Community structure: %d communities synced, %d still due", recorded, remaining)
		if !errors.Is(err, errSubGroupsRateLimited) {
			if err != nil {
				logger.Warnf("Community structure partly synced: %v", err)
			}
			return
		}
		logger.Warnf("%v; retrying in 15 minutes", err)
		time.Sleep(15 * time.Minute)
	}
}
