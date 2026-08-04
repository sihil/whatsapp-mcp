# Contact name resolution

Group messages currently cannot be attributed to a person. This design gives the
bridge a `contacts` table in `messages.db`, stops it from persisting phone
numbers as display names, and points the Python read layer at the result.

## Problem

Measured against the live store on 2026-08-04:

| | |
|---|---|
| Group messages from other people | 2,015 |
| …that resolve to a real name today | **0 (0.0%)** |
| Distinct group senders with no real name | 229 of 229 |
| `chats` rows whose `name` is a bare digit string | **227 of 370** |
| Missing names present in `whatsapp.db` already | **179 of 229 (78%)** |

Two independent defects produce this.

**No participant names are stored.** The read layer resolves a sender by looking
up `chats.name` for a row keyed by the sender's JID. That row exists only when
the sender also has a direct chat with the user, so group participants — the
majority of senders — have nothing to join against.

**`GetChatName` persists junk as a name.** Its individual-contact branch falls
back to `sender` and then `jid.User`, both bare digit strings, and `StoreChat`
uses `INSERT OR REPLACE`, so a later message overwrites a good name with digits.
The read layer cannot distinguish a digit string from a real name, so the
fallback chain terminates on garbage instead of continuing. Two rows are
actively wrong rather than merely unhelpful: chat `16508983112@s.whatsapp.net`
carries the name `180947643277487` (a LID stored as a person's name), and
`0@s.whatsapp.net` carries the name `0`.

The names needed to fix this are already on disk. `whatsmeow_contacts` in
`whatsapp.db` holds 1,789 rows, 996 with `full_name` and 1,017 with
`push_name`. The bridge queries that store for `/api/group/members` but persists
nothing. `msg.Info.PushName` also arrives free on every incoming message and is
discarded.

Note on a red herring: all 2,833 `messages.sender` values are clean phone-number
JIDs. JID canonicalization is working correctly and is not implicated.

## Approach

The bridge owns a `contacts` table in `messages.db`; Python reads it.

This preserves the existing split — the Go bridge is the only writer, Python
reads SQLite directly and needs no network — and keeps the read layer insulated
from whatsmeow's private schema, which gets bumped periodically (see the
`Client outdated (405)` note in `CLAUDE.md`).

Rejected: having Python `ATTACH` `whatsapp.db` and read `whatsmeow_contacts`
directly. It requires no Go changes and never goes stale, but couples the read
layer to an upstream schema that changes without notice; a column rename would
silently break name resolution.

## Schema

```sql
CREATE TABLE IF NOT EXISTS contacts (
    jid           TEXT PRIMARY KEY,   -- canonical PN JID, matches messages.sender
    full_name     TEXT,               -- address-book name
    push_name     TEXT,               -- self-set display name
    business_name TEXT
);
```

No `source` or `updated_at` column: nothing reads them, and name precedence is
already encoded by column order. Both are cheap to add if a refresh policy later
needs them.

`jid` is PN-canonical. Contact JIDs arrive in both the phone-number and LID
namespaces, so they pass through the existing `lidMapper` helpers in
`identity.go` before insert and join cleanly against `messages.sender`.

## Write paths

Three writers, each with different data quality. Every write is an upsert that
touches only the columns it actually knows, so a per-message `push_name` can
never clobber a good `full_name`:

```sql
INSERT INTO contacts (jid, push_name) VALUES (?, ?)
ON CONFLICT(jid) DO UPDATE SET
    push_name = CASE WHEN excluded.push_name != '' THEN excluded.push_name
                     ELSE contacts.push_name END;
```

| Writer | When | Columns |
|---|---|---|
| Startup backfill | every bridge start | all three, from `GetAllContacts` |
| Message handler | every incoming message | `push_name` from `msg.Info.PushName` |
| Group members endpoint | on `/api/group/members` | whatever the participant lookup resolved |

`GetAllContacts(ctx) (map[types.JID]types.ContactInfo, error)` is confirmed
present on `store.ContactStore` in the pinned whatsmeow. `ON CONFLICT` upserts
require SQLite 3.24+; go-sqlite3 v1.14.48 is well past that.

New code lives in `whatsapp-bridge/contacts.go` rather than growing `main.go`
(already 1,684 lines), following `identity.go`'s pattern of a narrow interface
over the whatsmeow dependency so the logic is testable without a live client.

## Stopping the junk names

- `GetChatName` returns `""` rather than a digit string. Groups keep their
  `Group <id>` fallback: human-readable, not a bare number.
- `StoreChat` becomes an upsert that preserves an existing name when the
  incoming one is empty.
- The startup migration nulls the 227 existing numeric names.

Scrubbing alone would leave those 227 chats displaying as nameless, so it is
only correct together with the read-layer change below.

## Read layer

`db.py::resolve_sender_names` gains one bulk `LEFT JOIN` against `contacts`,
resolving a sender in this order:

1. `contacts.full_name`
2. `contacts.push_name`
3. `contacts.business_name`
4. `chats.name`
5. the existing anchored-LIKE fallback
6. the JID itself

A shared `GLOB '*[^0-9]*'` predicate rejects digit-only strings at every step,
so junk cannot resurface from a database written by an older bridge.

`db_chats.py::_chat_query` coalesces `chats.name` with the contact name. For a
direct chat the chat JID *is* the contact JID, so this restores the scrubbed
names from the real source instead of from a persisted guess.

## Migration

One idempotent startup pass beside `canonicalizeMessageStore`: create the table,
sync from the contact store, scrub numeric chat names. Re-running is a no-op;
running against a fresh database is a no-op. No network calls.

Populating rosters for participants who have never sent a message and are absent
from the contact store would require `GetGroupInfo` calls at startup. That is
out of scope here — it adds startup latency and rate-limit exposure for a
long-tail gain.

## Testing

`whatsapp-bridge/contacts_test.go`, alongside `identity_test.go`:

- upsert precedence across the three writers
- a push-name-only write preserves an existing `full_name`
- the numeric scrub nulls digit strings and leaves real names alone
- running the migration twice produces the same result as running it once
- `GetChatName` never returns a bare digit string

There are no Python tests in this repo and none are added. The Python change is
verified by re-running the attribution query against the real store and
reporting the measured before/after, not the 78% estimate.

## Success criteria

- Group messages from other people resolve to a real name at a rate materially
  above 0%, measured on the live store.
- No `chats.name` value is a bare digit string.
- `go test ./...` passes in `whatsapp-bridge/`.
- Reads still work with the bridge stopped.
