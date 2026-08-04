# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

WhatsApp MCP server: lets an MCP client (Claude Desktop, Cursor) search/read personal WhatsApp messages and send messages/media. Connects to the user's personal WhatsApp account via the WhatsApp web multidevice API.

## Architecture

Two cooperating processes; both must be running for the system to work:

1. **`whatsapp-bridge/`** — Go application (single file, `main.go`) using [whatsmeow](https://github.com/tulir/whatsmeow). Connects to WhatsApp, authenticates via QR code, stores message history in SQLite, and exposes a REST API on `localhost:8080`:
   - `POST /api/send` — send a text message or media file; optional `reply_to` (a message ID) sends it as a quoted reply
   - `POST /api/download` — download media from a message to disk
   - `POST /api/group/members` — list group members (with contact-name lookup)

2. **`whatsapp-mcp-server/`** — Python MCP server (FastMCP, stdio transport):
   - `main.py` — thin `@mcp.tool()` wrappers, one per tool
   - `whatsapp.py` — all implementation: dataclasses, SQLite queries, REST calls to the bridge
   - `audio.py` — FFmpeg conversion of audio to `.ogg` Opus for voice messages

**Data flow is split by operation type:**
- **Reads** (list/search messages, chats, contacts): Python queries the bridge's SQLite DB **directly** at `whatsapp-bridge/store/messages.db` (path computed relative to `whatsapp.py`). The Go bridge is not involved.
- **Writes/live actions** (send message/file/audio, download media, group members): Python calls the bridge's REST API at `http://localhost:8080/api`, which talks to WhatsApp.

**Adding a tool that needs live WhatsApp data touches three places** (see recent group-members commits as a template): an endpoint handler in `whatsapp-bridge/main.go`, an implementation function in `whatsapp.py` that calls it, and an `@mcp.tool()` wrapper in `main.py`.

### Storage

- `whatsapp-bridge/store/messages.db` — chats + messages + jid_aliases tables (created by `NewMessageStore` in `main.go`; schema changes go there)
- `whatsapp-bridge/store/whatsapp.db` — whatsmeow session/device state
- Both are gitignored. Deleting both forces re-authentication and a fresh history sync.
- Media is stored as metadata only (url, media_key, hashes); actual files are fetched on demand via the `download_media` tool.
- **JID canonicalization** (`whatsapp-bridge/identity.go`): `messages.sender` and `chat_jid` always hold full `user@server` JIDs, phone-number namespace preferred, `@lid` only when the PN↔LID mapping is unknown, own PN JID for `is_from_me` rows. An idempotent migration runs at every bridge startup to canonicalize legacy rows and merge `@lid`-keyed DM chats into their PN keys; renamed chat keys leave a row in `jid_aliases` that the Python read layer follows for stale keys. Don't write raw `msg.Info` JIDs to the DB — go through the `canonical*` helpers.

## Commands

```bash
# Run the Go bridge (must run from whatsapp-bridge/ so store/ resolves correctly)
cd whatsapp-bridge && go run main.go

# Build the bridge binary (gitignored)
cd whatsapp-bridge && go build

# Run the MCP server standalone (normally launched by Claude Desktop/Cursor via stdio)
cd whatsapp-mcp-server && uv run main.py
```

Python deps are managed with `uv` (`pyproject.toml` + `uv.lock`, Python ≥3.11). There are no tests or linters configured in this repo.

## Gotchas

- First bridge run prints a QR code to scan with the WhatsApp mobile app; re-auth is needed roughly every 20 days. If messages get out of sync, delete both files in `whatsapp-bridge/store/` and restart the bridge.
- A `Client outdated (405) connect failure` on startup means the pinned whatsmeow is too old — it embeds a WhatsApp Web client version that WhatsApp's servers reject after a few months. Fix: `go get go.mau.fi/whatsmeow@latest && go mod tidy` in `whatsapp-bridge/`. This recurs periodically.
- `go-sqlite3` requires CGO; on Windows CGO is disabled by default (`go env -w CGO_ENABLED=1` plus a C compiler).
- FFmpeg is optional and only needed by `send_audio_message` to convert non-Opus audio; without it, raw audio can still be sent via `send_file`.
- Recipients are either a bare phone number (country code, no `+`) or a JID (`...@s.whatsapp.net` for individuals, `...@g.us` for groups). Group operations require the JID.
- The messages DB timestamps and queries assume ISO-8601 strings; `list_messages` date filters must be ISO-8601.
