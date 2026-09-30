# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

WhatsApp MCP server: lets an MCP client (Claude Desktop, Cursor) search/read personal WhatsApp messages and send messages/media. Connects to the user's personal WhatsApp account via the WhatsApp web multidevice API.

## Architecture

Two cooperating processes; both must be running for the system to work:

1. **`whatsapp-bridge/`** — Go application (`main.go`: bridge, REST API, message store; `identity.go`: JID canonicalization; Go tests alongside) using [whatsmeow](https://github.com/tulir/whatsmeow). Connects to WhatsApp, authenticates via QR code (or a pairing code via `-pair-phone`, see Commands/Gotchas), stores message history in SQLite, and exposes a REST API on `localhost:8080`. The client sets `SynchronousAck` (messages are only acked after the SQLite write) and `UseRetryMessageStore` (outbound messages survive decryption-retry requests across restarts):
   - `POST /api/send` — send a text message or media file; optional `reply_to` (a message ID) sends it as a quoted reply
   - `POST /api/react` / `POST /api/edit` / `POST /api/delete` (`api_mutations.go`) — react to a message (empty `emoji` removes the reaction), edit one of your own text messages, delete one of your own messages for everyone. All answer `{"success","message"}` (400 bad input or refused, 500 not connected/send failure) and look the target up in `messages` first (unknown → "message not found"). Edit is refused for non-own, media, deleted, or older-than-15-minutes messages (WhatsApp's edit window); the store change is applied right after a successful send because the sending device gets no echo
   - `POST /api/download` — download media from a message to disk
   - `POST /api/group/members` — list group members (with contact-name lookup)

2. **`whatsapp-mcp-server/`** — Python MCP server (`mcp` SDK v2 `MCPServer`, stdio transport), split into small modules (each ≤300 lines, see `whatsapp-mcp-server/README.md` for the full breakdown).

**Data flow is split by operation type:**
- **Reads** (list/search messages, chats, contacts): Python queries the bridge's SQLite DB **directly** at `whatsapp-bridge/store/messages.db` (path computed relative to `config.py`; override with `WHATSAPP_MCP_DB_PATH`). The Go bridge is not involved.
- **Writes/live actions** (send message/file/audio, download media, group members): Python calls the bridge's REST API at `http://localhost:8080/api` (override with `WHATSAPP_API_BASE_URL`), which talks to WhatsApp.

Other env vars (all read in `config.py`): `WHATSAPP_MCP_ALLOWED_MEDIA_DIRS` (allowlist for outbound media paths, separated by `os.pathsep` — `:` on Unix, `;` on Windows; unset = any existing file), `WHATSAPP_MCP_SEND_RATE_LIMIT` (max sends/minute across the send tools plus `send_reaction`/`edit_message`/`delete_message`, default 30, `0` disables), `WHATSAPP_MCP_LOG_LEVEL` (stderr log level, default INFO — stdout carries only JSON-RPC).

**Adding a tool that needs live WhatsApp data touches four places** (see recent group-members commits as a template): an endpoint handler in `whatsapp-bridge/main.go`, an implementation function in `whatsapp-mcp-server/api_client.py` that calls it, a re-export in `whatsapp.py`, and an `@mcp.tool()` wrapper in `tools_send.py` (read-only tools go in `tools_read.py`, backed by `db_chats.py`/`db_contacts.py`/`db_messages.py`). The react/edit/delete tools follow it with `api_actions.py` (sharing `api_client`'s rate limiter and transport) in place of `api_client.py`, and their handlers live in `api_mutations.go`. On the read side every message dict gains `edited`, `deleted` and `reactions`; `db_reactions.py` fetches a listing's reactions in one query, and the read layer probes once per request for the columns/table so a store from an older bridge reads as unedited/undeleted/no reactions.

### Storage

- `whatsapp-bridge/store/messages.db` — chats + messages + jid_aliases + contacts tables (created by `NewMessageStore` in `main.go`; schema changes go there)
- `whatsapp-bridge/store/whatsapp.db` — whatsmeow session/device state
- Both are gitignored. Deleting both forces re-authentication and a fresh history sync.
- **Edits, deletes, reactions** (`mutations.go` decision logic + store methods, `mutation_events.go` incoming events, `mutation_store.go` upkeep; the columns/table are added by the idempotent `migrateMutations` at startup and also sit in the fresh-store `CREATE TABLE`). `messages.edited_at`/`deleted_at` are nullable. An edit rewrites `content` and stamps `edited_at`, only when the editor is the stored sender and the new text is non-empty. A delete ("revoke") keeps the row as a **tombstone**: `content = ''`, `deleted_at` set, `url`/`media_key`/hashes/`file_length` NULLed, but `media_type` and `filename` kept so the row isn't mistaken for empty; its `reactions` rows are dropped and later reactions to it are ignored. The stored sender may always revoke; in a group another participant's revoke is applied only after `GetGroupInfo` confirms they are an admin (`groupAdminCheck`), because the stanza is end-to-end encrypted and proves nothing about the revoker — without the check any member could erase anyone's messages from the store. Unknown targets are no-ops. `reactions` holds one row per `(message_id, chat_jid, sender)` with canonical JIDs (own reactions use the own PN JID); an empty emoji deletes the row, `''` is never stored. Reactions, edits and revokes arrive as `*events.Message` and are dispatched in `handleMessage` after the canonical JIDs are computed and before the empty-content early exit, so they never become ordinary messages or bump chat time; history sync carries reactions on `WebMessageInfo` and a `REVOKE` stub type marks a deleted message. Startup canonicalization also rewrites `reactions.chat_jid`/`sender`.
- **`StoreMessage` is an upsert, not `INSERT OR REPLACE`**: a re-delivered message (history sync) must not wipe `edited_at`/`deleted_at`. A row with `deleted_at` set is left alone (a tombstone is never resurrected) and an edited row keeps its edited `content`.
- **Bridge sends are stored by the bridge.** The sending device gets no echo, so `/api/send` writes the sent row itself (`recordSentMessage`) and returns the new message ID in its message; without a row there is nothing for `edit_message`/`delete_message`/`send_reaction` to target. `download_media` refuses a tombstone.
- Media is stored as metadata only (url, media_key, hashes); actual files are fetched on demand via the `download_media` tool.
- **Contact names** (`whatsapp-bridge/contacts.go`): the `contacts` table maps a canonical PN JID to `full_name`/`push_name`/`business_name`. Without it a group participant is nameless, because `chats` holds a row per *conversation*, not per person. Three writers fill it — a backfill from whatsmeow's contact store at every startup, `msg.Info.PushName` on each incoming message, and the group-members endpoint — and each upserts only the columns it knows, so a push name never overwrites an address-book name. **A name that is nothing but digits is not a name** — nor is a blank. `GetChatName` used to fall back to `jid.User`, and the read layer cannot tell such a value from a real name, so it stops its fallback chain on it; both sides now reject them (`keepName` in Go, `named()` in `db_names.py`) and the startup migration clears the stored ones. **Groups are exempt**: a group name is typed by a person and has no contact behind a `@g.us` JID to fall back to, so "2024" is a real group name — go through `keepChatName`, not `keepName`, for anything keyed by a chat JID.
- **Display-name precedence** is one rule in two places, and the two must agree: a contact's `full_name`, then the name on the chat, then `push_name`, then `business_name`. Go expresses it in `bestContactName`/`contactNamesOf` (`contacts.go`), Python in `db_names.py` plus `resolve_sender_names`. `push_name` ranks below the chat's own name because the sender chooses it themselves.
- **JID canonicalization** (`whatsapp-bridge/identity.go`): `messages.sender` and `chat_jid` always hold full `user@server` JIDs, phone-number namespace preferred, `@lid` only when the PN↔LID mapping is unknown, own PN JID for `is_from_me` rows. An idempotent migration runs at every bridge startup to canonicalize legacy rows and merge `@lid`-keyed DM chats into their PN keys; renamed chat keys leave a row in `jid_aliases` that the Python read layer follows for stale keys. Don't write raw `msg.Info` JIDs to the DB — go through the `canonical*` helpers.

## Commands

```bash
# Run the Go bridge (must run from whatsapp-bridge/ so store/ resolves correctly)
cd whatsapp-bridge && go run .

cd whatsapp-bridge && go run . -pair-phone 15551234567  # pair by 8-char code instead of QR

# Run the MCP server standalone (normally launched by Claude Desktop/Cursor via stdio)
cd whatsapp-mcp-server && uv run main.py
```

Python deps are managed with `uv` (`pyproject.toml` + `uv.lock`, Python ≥3.11). The Go bridge has tests (`go test ./...`); there are no Python tests and no linters configured.

## Gotchas

- First bridge run prints a QR code to scan with the WhatsApp mobile app; re-auth is needed roughly every 20 days. If messages get out of sync, delete both files in `whatsapp-bridge/store/` and restart the bridge. Pass `-pair-phone <number>` (country code, digits only, no `+`) to get an 8-character code to type into the phone instead of scanning a QR — useful for headless re-auth. Ignored if a session already exists. The bridge appears as "WhatsApp MCP Bridge" in the phone's Linked Devices list — this needs both `DeviceProps.Os` and `DeviceProps.PlatformType` set (the phone shows "Other device" when PlatformType is UNKNOWN), and only takes effect at pairing time.
- Only one bridge can run per session. The bridge binds port 8080 before connecting (`listenLocal` in `lifecycle.go`), so a second copy exits with `Another bridge appears to be running` instead of logging in and knocking the first offline; if the session is taken over anyway (`StreamReplaced`), the bridge exits non-zero for its supervisor to restart. If a supervisor (e.g. a launchd agent) runs a prebuilt binary, stop it before `go run .` and rebuild the binary after source changes.
- A `Client outdated (405) connect failure` on startup means the pinned whatsmeow is too old — it embeds a WhatsApp Web client version that WhatsApp's servers reject after a few months. Fix: `go get go.mau.fi/whatsmeow@latest && go mod tidy` in `whatsapp-bridge/`. This recurs periodically.
- `go-sqlite3` requires CGO; on Windows CGO is disabled by default (`go env -w CGO_ENABLED=1` plus a C compiler).
- FFmpeg is optional and only needed by `send_audio_message` to convert non-Opus audio; without it, raw audio can still be sent via `send_file`.
- Recipients are either a bare phone number (country code, no `+`) or a JID (`...@s.whatsapp.net` for individuals, `...@g.us` for groups). Group operations require the JID.
- The messages DB timestamps and queries assume ISO-8601 strings; `list_messages` date filters must be ISO-8601.
