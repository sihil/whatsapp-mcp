package main

import (
	"context"
	"database/sql"
	"testing"

	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type fakeLIDMapper struct {
	pnForLID map[string]types.JID
	lidForPN map[string]types.JID
}

func (f fakeLIDMapper) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f.pnForLID[lid.String()], nil
}

func (f fakeLIDMapper) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	return f.lidForPN[pn.String()], nil
}

func pnJID(user string) types.JID  { return types.JID{User: user, Server: types.DefaultUserServer} }
func lidJID(user string) types.JID { return types.JID{User: user, Server: types.HiddenUserServer} }

func TestCanonicalSenderJID(t *testing.T) {
	own := pnJID("15550009999")
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"300000000000001@lid": pnJID("15550003001"),
	}}

	tests := []struct {
		name      string
		sender    types.JID
		senderAlt types.JID
		isFromMe  bool
		want      string
	}{
		{
			name:   "pn sender is stored as its non-AD JID",
			sender: types.JID{User: "15550001001", Device: 3, Server: types.DefaultUserServer},
			want:   "15550001001@s.whatsapp.net",
		},
		{
			name:      "lid sender with pn alt uses the alt",
			sender:    types.JID{User: "300000000000009", Device: 7, Server: types.HiddenUserServer},
			senderAlt: types.JID{User: "15550001002", Device: 7, Server: types.DefaultUserServer},
			want:      "15550001002@s.whatsapp.net",
		},
		{
			name:   "lid sender without alt resolves via the lid store",
			sender: lidJID("300000000000001"),
			want:   "15550003001@s.whatsapp.net",
		},
		{
			name:   "unmapped lid sender keeps its full lid JID",
			sender: lidJID("300000000000002"),
			want:   "300000000000002@lid",
		},
		{
			name:     "own message stores the own pn JID even when sent under lid",
			sender:   lidJID("999999999999999"),
			isFromMe: true,
			want:     "15550009999@s.whatsapp.net",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalSenderJID(context.Background(), lids, tt.sender, tt.senderAlt, own, tt.isFromMe)
			if got != tt.want {
				t.Errorf("canonicalSenderJID(%v, %v, %v) = %q, want %q", tt.sender, tt.senderAlt, tt.isFromMe, got, tt.want)
			}
		})
	}
}

func TestCanonicalChatJID(t *testing.T) {
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"200000000000001@lid": pnJID("15550002001"),
	}}

	tests := []struct {
		name string
		chat types.JID
		alt  types.JID
		want types.JID
	}{
		{
			name: "group chat passes through",
			chat: types.JID{User: "123456789", Server: types.GroupServer},
			want: types.JID{User: "123456789", Server: types.GroupServer},
		},
		{
			name: "pn DM passes through",
			chat: pnJID("15550001001"),
			want: pnJID("15550001001"),
		},
		{
			name: "status broadcast passes through",
			chat: types.JID{User: "status", Server: types.BroadcastServer},
			want: types.JID{User: "status", Server: types.BroadcastServer},
		},
		{
			name: "lid DM with pn alt uses the alt",
			chat: lidJID("200000000000009"),
			alt:  types.JID{User: "15550001002", Device: 2, Server: types.DefaultUserServer},
			want: pnJID("15550001002"),
		},
		{
			name: "lid DM without alt resolves via the lid store",
			chat: lidJID("200000000000001"),
			want: pnJID("15550002001"),
		},
		{
			name: "unmapped lid DM stays lid-keyed",
			chat: lidJID("200000000000002"),
			want: lidJID("200000000000002"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalChatJID(context.Background(), lids, tt.chat, tt.alt)
			if got != tt.want {
				t.Errorf("canonicalChatJID(%v, %v) = %v, want %v", tt.chat, tt.alt, got, tt.want)
			}
		})
	}
}

func TestHistoryPNMap(t *testing.T) {
	data := &waHistorySync.HistorySync{
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{
			{PnJID: proto.String("15550001001@c.us"), LidJID: proto.String("300000000000001@lid")},
			{PnJID: proto.String("15550001002@s.whatsapp.net"), LidJID: proto.String("300000000000002@lid")},
			{PnJID: proto.String(""), LidJID: proto.String("300000000000003@lid")},
		},
	}

	m := historyPNMap(data)

	if got := m["300000000000001@lid"]; got != pnJID("15550001001") {
		t.Errorf("c.us mapping = %v, want %v (normalized to s.whatsapp.net)", got, pnJID("15550001001"))
	}
	if got := m["300000000000002@lid"]; got != pnJID("15550001002") {
		t.Errorf("plain mapping = %v, want %v", got, pnJID("15550001002"))
	}
	if _, ok := m["300000000000003@lid"]; ok {
		t.Errorf("mapping with empty pn side should be skipped")
	}
}

func TestCanonicalHistoryChatJID(t *testing.T) {
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"200000000000002@lid": pnJID("15550002002"),
	}}
	pnMap := map[string]types.JID{
		"200000000000001@lid": pnJID("15550002001"),
	}

	tests := []struct {
		name      string
		chat      types.JID
		convPnJID string
		want      types.JID
	}{
		{
			name: "group conversation passes through",
			chat: types.JID{User: "123456789", Server: types.GroupServer},
			want: types.JID{User: "123456789", Server: types.GroupServer},
		},
		{
			name:      "lid conversation prefers the conversation's own pn JID",
			chat:      lidJID("200000000000009"),
			convPnJID: "15550009001@s.whatsapp.net",
			want:      pnJID("15550009001"),
		},
		{
			name: "lid conversation falls back to the payload mapping table",
			chat: lidJID("200000000000001"),
			want: pnJID("15550002001"),
		},
		{
			name: "lid conversation falls back to the lid store last",
			chat: lidJID("200000000000002"),
			want: pnJID("15550002002"),
		},
		{
			name: "unmapped lid conversation stays lid-keyed",
			chat: lidJID("200000000000003"),
			want: lidJID("200000000000003"),
		},
		{
			name: "legacy c.us conversation is normalized",
			chat: types.JID{User: "15550001001", Server: "c.us"},
			want: pnJID("15550001001"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalHistoryChatJID(context.Background(), lids, tt.chat, tt.convPnJID, pnMap)
			if got != tt.want {
				t.Errorf("canonicalHistoryChatJID(%v, %q) = %v, want %v", tt.chat, tt.convPnJID, got, tt.want)
			}
		})
	}
}

func TestCanonicalHistorySenderJID(t *testing.T) {
	own := pnJID("15550009999")
	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"300000000000002@lid": pnJID("15550003002"),
	}}
	pnMap := map[string]types.JID{
		"300000000000001@lid": pnJID("15550003001"),
	}
	groupChat := types.JID{User: "123456789", Server: types.GroupServer}
	dmChat := pnJID("15550001001")

	tests := []struct {
		name        string
		participant string
		isFromMe    bool
		chat        types.JID
		emptyOwn    bool
		want        string
	}{
		{
			name:     "own row stores the own pn JID",
			isFromMe: true,
			chat:     groupChat,
			want:     "15550009999@s.whatsapp.net",
		},
		{
			name:        "own row ignores whatever participant says",
			participant: "300000000000001@lid",
			isFromMe:    true,
			chat:        groupChat,
			want:        "15550009999@s.whatsapp.net",
		},
		{
			name:        "lid participant resolves via the payload mapping first",
			participant: "300000000000001@lid",
			chat:        groupChat,
			want:        "15550003001@s.whatsapp.net",
		},
		{
			name:        "lid participant falls back to the lid store",
			participant: "300000000000002@lid",
			chat:        groupChat,
			want:        "15550003002@s.whatsapp.net",
		},
		{
			name:        "unmapped lid participant keeps its full lid JID",
			participant: "300000000000003@lid",
			chat:        groupChat,
			want:        "300000000000003@lid",
		},
		{
			name:        "legacy c.us participant is normalized",
			participant: "15550004001@c.us",
			chat:        groupChat,
			want:        "15550004001@s.whatsapp.net",
		},
		{
			name:        "device-suffixed participant is stored non-AD",
			participant: "15550004002:12@s.whatsapp.net",
			chat:        groupChat,
			want:        "15550004002@s.whatsapp.net",
		},
		{
			name: "DM row with no participant stores the canonical chat JID",
			chat: dmChat,
			want: "15550001001@s.whatsapp.net",
		},
		{
			name:     "own row with unknown own JID falls back to the chat peer instead of an empty sender",
			isFromMe: true,
			chat:     dmChat,
			emptyOwn: true,
			want:     "15550001001@s.whatsapp.net",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ownArg := own
			if tt.emptyOwn {
				ownArg = types.JID{}
			}
			got := canonicalHistorySenderJID(context.Background(), lids, tt.participant, tt.isFromMe, tt.chat, ownArg, pnMap)
			if got != tt.want {
				t.Errorf("canonicalHistorySenderJID(%q, %v, %v) = %q, want %q", tt.participant, tt.isFromMe, tt.chat, got, tt.want)
			}
		})
	}
}

func TestResolveReplyParticipant(t *testing.T) {
	groupJID := types.JID{User: "120363000000000000", Server: types.GroupServer}
	dmJID := pnJID("16465559999")
	ownJID := pnJID("15550001111")
	lids := fakeLIDMapper{
		lidForPN: map[string]types.JID{
			"16175551234@s.whatsapp.net": lidJID("113108114763976"),
			"15550001111@s.whatsapp.net": lidJID("888000000000000"),
		},
	}

	tests := []struct {
		name         string
		storedSender string
		isFromMe     bool
		chatJID      types.JID
		want         types.JID
	}{
		{
			name:         "lid sender is used as-is",
			storedSender: "74616466886757@lid",
			chatJID:      groupJID,
			want:         lidJID("74616466886757"),
		},
		{
			name:         "pn sender in a group prefers its lid form",
			storedSender: "16175551234@s.whatsapp.net",
			chatJID:      groupJID,
			want:         lidJID("113108114763976"),
		},
		{
			name:         "pn sender in a group without a lid mapping stays pn",
			storedSender: "16465551234@s.whatsapp.net",
			chatJID:      groupJID,
			want:         pnJID("16465551234"),
		},
		{
			name:         "pn sender in a DM stays pn even when a lid mapping exists",
			storedSender: "16175551234@s.whatsapp.net",
			chatJID:      dmJID,
			want:         pnJID("16175551234"),
		},
		{
			name:         "legacy bare-digit sender in a DM resolves to the chat JID",
			storedSender: "16465559999",
			chatJID:      dmJID,
			want:         dmJID,
		},
		{
			name:         "legacy bare-digit sender in a group falls back to the default user server",
			storedSender: "16465551234",
			chatJID:      groupJID,
			want:         pnJID("16465551234"),
		},
		{
			name:         "own message in a group prefers the own lid form",
			storedSender: "15550001111@s.whatsapp.net",
			isFromMe:     true,
			chatJID:      groupJID,
			want:         lidJID("888000000000000"),
		},
		{
			name:         "own message in a DM stays the own pn JID",
			storedSender: "15550001111@s.whatsapp.net",
			isFromMe:     true,
			chatJID:      dmJID,
			want:         ownJID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReplyParticipant(context.Background(), lids, tt.storedSender, tt.isFromMe, tt.chatJID, ownJID)
			if got != tt.want {
				t.Errorf("resolveReplyParticipant(%q, %v, %v, %v) = %v, want %v",
					tt.storedSender, tt.isFromMe, tt.chatJID, ownJID, got, tt.want)
			}
		})
	}
}

func TestCanonicalizeMessageStore(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("opening in-memory db: %v", err)
	}
	defer db.Close()
	// Mirror production: NewMessageStore opens the DB with _foreign_keys=on
	// and messages.chat_jid references chats(jid).
	for _, ddl := range []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("creating schema: %v", err)
		}
	}

	lids := fakeLIDMapper{pnForLID: map[string]types.JID{
		"200000000000001@lid": pnJID("15550002001"),
		"200000000000002@lid": pnJID("15550002002"),
		"200000000000003@lid": pnJID("15550002003"),
		"300000000000001@lid": pnJID("15550003001"),
		"400000000000001@lid": pnJID("15550004001"),
	}}
	own := pnJID("15550009999")

	chats := [][3]string{
		{"200000000000001@lid", "Alice", "2026-01-02 00:00:00"},
		{"200000000000002@lid", "Bob", "2026-01-05 00:00:00"},
		{"15550002002@s.whatsapp.net", "", "2026-01-01 00:00:00"},
		{"500000000000009@lid", "Carol", "2026-01-03 00:00:00"},
		{"123456789@g.us", "Group", "2026-01-04 00:00:00"},
		// DST straddle: the @lid row's timestamp is chronologically later
		// (06:05 UTC vs 05:45 UTC) but textually smaller — a plain MAX()
		// on the TEXT column would keep the wrong one.
		{"200000000000003@lid", "Dave", "2025-11-02 01:05:00-05:00"},
		{"15550002003@s.whatsapp.net", "", "2025-11-02 01:45:00-04:00"},
		// Legacy pre-multidevice chat key.
		{"15550007001@c.us", "Eve", "2026-01-06 00:00:00"},
		{"111222333@broadcast", "Broadcast", "2026-01-07 00:00:00"},
	}
	for _, c := range chats {
		if _, err := db.Exec("INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", c[0], c[1], c[2]); err != nil {
			t.Fatalf("inserting chat: %v", err)
		}
	}

	messages := []struct {
		id, chat, sender, content string
		isFromMe                  bool
	}{
		{"m1", "200000000000001@lid", "200000000000001", "hi", false},
		{"m2", "200000000000002@lid", "200000000000002", "from lid row", false},
		{"m2", "15550002002@s.whatsapp.net", "15550002002", "from pn row", false},
		{"m2b", "200000000000002@lid", "200000000000002", "only lid", false},
		{"m3", "123456789@g.us", "300000000000001@lid", "mapped lid sender", false},
		{"m4", "123456789@g.us", "400000000000001", "mapped bare lid digits", false},
		{"m5", "123456789@g.us", "600000000000001", "unmapped bare digits", false},
		{"m6", "123456789@g.us", "700000000000001@lid", "unmapped lid sender", false},
		{"m7", "123456789@g.us", "15550005001@c.us", "legacy c.us", false},
		{"m8", "15550002002@s.whatsapp.net", "15550009999", "own bare digits", true},
		{"m9", "500000000000009@lid", "500000000000009", "unmapped lid dm", false},
		{"m10", "123456789@g.us", "15550006001:12@s.whatsapp.net", "device suffix", false},
		{"m11", "15550007001@c.us", "15550007001", "legacy c.us chat", false},
		{"m12", "111222333@broadcast", "15550008001", "bare broadcast sender", false},
	}
	for _, m := range messages {
		if _, err := db.Exec("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, '2026-01-01 00:00:00', ?)",
			m.id, m.chat, m.sender, m.content, m.isFromMe); err != nil {
			t.Fatalf("inserting message: %v", err)
		}
	}

	changed, err := canonicalizeMessageStore(context.Background(), db, lids, own)
	if err != nil {
		t.Fatalf("canonicalizeMessageStore: %v", err)
	}
	if changed == 0 {
		t.Fatalf("first run reported no changes")
	}

	wantMessages := map[string][2]string{ // id -> {chat_jid, sender}
		"m1":  {"15550002001@s.whatsapp.net", "15550002001@s.whatsapp.net"},
		"m2":  {"15550002002@s.whatsapp.net", "15550002002@s.whatsapp.net"},
		"m2b": {"15550002002@s.whatsapp.net", "15550002002@s.whatsapp.net"},
		"m3":  {"123456789@g.us", "15550003001@s.whatsapp.net"},
		"m4":  {"123456789@g.us", "15550004001@s.whatsapp.net"},
		"m5":  {"123456789@g.us", "600000000000001@s.whatsapp.net"},
		"m6":  {"123456789@g.us", "700000000000001@lid"},
		"m7":  {"123456789@g.us", "15550005001@s.whatsapp.net"},
		"m8":  {"15550002002@s.whatsapp.net", "15550009999@s.whatsapp.net"},
		"m9":  {"500000000000009@lid", "500000000000009@lid"},
		"m10": {"123456789@g.us", "15550006001@s.whatsapp.net"},
		"m11": {"15550007001@s.whatsapp.net", "15550007001@s.whatsapp.net"},
		"m12": {"111222333@broadcast", "15550008001@s.whatsapp.net"},
	}
	for id, want := range wantMessages {
		var chat, sender string
		if err := db.QueryRow("SELECT chat_jid, sender FROM messages WHERE id = ?", id).Scan(&chat, &sender); err != nil {
			t.Errorf("message %s: %v", id, err)
			continue
		}
		if chat != want[0] || sender != want[1] {
			t.Errorf("message %s = (%s, %s), want (%s, %s)", id, chat, sender, want[0], want[1])
		}
	}

	// The colliding m2 pair keeps the row that already lived under the PN key.
	var content string
	if err := db.QueryRow("SELECT content FROM messages WHERE id = 'm2'").Scan(&content); err != nil || content != "from pn row" {
		t.Errorf("m2 content = (%q, %v), want the pre-existing pn row to survive", content, err)
	}
	var m2Count int
	db.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 'm2'").Scan(&m2Count)
	if m2Count != 1 {
		t.Errorf("m2 row count = %d, want 1 after merge", m2Count)
	}

	// go-sqlite3 renders TIMESTAMP columns as RFC3339 when scanned into strings.
	wantChats := map[string][2]string{ // jid -> {name, last_message_time}
		"15550002001@s.whatsapp.net": {"Alice", "2026-01-02T00:00:00Z"},
		"15550002002@s.whatsapp.net": {"Bob", "2026-01-05T00:00:00Z"},
		"500000000000009@lid":        {"Carol", "2026-01-03T00:00:00Z"},
		"123456789@g.us":             {"Group", "2026-01-04T00:00:00Z"},
		"15550002003@s.whatsapp.net": {"Dave", "2025-11-02T01:05:00-05:00"},
		"15550007001@s.whatsapp.net": {"Eve", "2026-01-06T00:00:00Z"},
		"111222333@broadcast":        {"Broadcast", "2026-01-07T00:00:00Z"},
	}
	var chatCount int
	db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chatCount)
	if chatCount != len(wantChats) {
		t.Errorf("chat count = %d, want %d", chatCount, len(wantChats))
	}
	for jid, want := range wantChats {
		var name, lmt string
		if err := db.QueryRow("SELECT name, last_message_time FROM chats WHERE jid = ?", jid).Scan(&name, &lmt); err != nil {
			t.Errorf("chat %s: %v", jid, err)
			continue
		}
		if name != want[0] || lmt != want[1] {
			t.Errorf("chat %s = (%q, %q), want (%q, %q)", jid, name, lmt, want[0], want[1])
		}
	}

	// Merged and renamed keys leave an alias behind so readers holding a
	// stale key can follow it.
	wantAliases := map[string]string{
		"200000000000001@lid": "15550002001@s.whatsapp.net",
		"200000000000002@lid": "15550002002@s.whatsapp.net",
		"200000000000003@lid": "15550002003@s.whatsapp.net",
		"15550007001@c.us":    "15550007001@s.whatsapp.net",
	}
	var aliasCount int
	db.QueryRow("SELECT COUNT(*) FROM jid_aliases").Scan(&aliasCount)
	if aliasCount != len(wantAliases) {
		t.Errorf("alias count = %d, want %d", aliasCount, len(wantAliases))
	}
	for old, want := range wantAliases {
		var got string
		if err := db.QueryRow("SELECT new_jid FROM jid_aliases WHERE old_jid = ?", old).Scan(&got); err != nil || got != want {
			t.Errorf("alias %s = (%q, %v), want %q", old, got, err, want)
		}
	}

	// Idempotent: a second run changes nothing.
	changed, err = canonicalizeMessageStore(context.Background(), db, lids, own)
	if err != nil {
		t.Fatalf("second canonicalizeMessageStore: %v", err)
	}
	if changed != 0 {
		t.Errorf("second run changed %d rows, want 0", changed)
	}
}
