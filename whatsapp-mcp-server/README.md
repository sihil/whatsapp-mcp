# whatsapp-mcp-server

Python MCP server (`mcp` SDK v2 `MCPServer`, stdio transport) that exposes WhatsApp search/read/send
tools to an MCP client (Claude Desktop, Cursor). It works together with the Go
bridge in `../whatsapp-bridge/` — see the repo-root `README.md` for the full setup
(running the bridge, QR/pairing auth) and the repo-root `CLAUDE.md` for architecture
notes.

## Data flow

- **Reads** (`search_contacts`, `list_messages`, `list_chats`, `get_chat`,
  `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`,
  `get_message_context`) query the bridge's SQLite database **directly**,
  read-only. The Go bridge does not need to be running for these to work, as
  long as `messages.db` already exists.
- **Writes / live actions** (`send_message`, `send_file`, `send_audio_message`,
  `send_reaction`, `edit_message`, `delete_message`, `download_media`,
  `list_group_members`) call the bridge's REST API at
  `http://localhost:8080/api` (or `WHATSAPP_API_BASE_URL`), which talks to
  WhatsApp. The bridge must be running for these.

## Running

Normally launched by an MCP client via stdio, not run directly. Example Claude
Desktop / Cursor config entry:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/path/to/uv",
      "args": [
        "--directory",
        "/path/to/whatsapp-mcp/whatsapp-mcp-server",
        "run",
        "main.py"
      ],
      "env": {
        "WHATSAPP_MCP_ALLOWLIST": "/path/to/family.allowlist.json"
      }
    }
  }
}
```

To run it standalone (e.g. for debugging):

```bash
uv run main.py
```

## Data paths

- `messages.db` — read directly by this server (`db.py`). Default location:
  `../whatsapp-bridge/store/messages.db`, resolved relative to this directory.
  Contains the `chats`, `messages`, `jid_aliases`, and `contacts` tables
  written by the Go bridge. Override with `WHATSAPP_MCP_DB_PATH`.

  Messages carry `edited`/`deleted` (from `messages.edited_at`/`deleted_at`) and
  `reactions` (from the `reactions` table); a store from an older bridge lacking
  them reads as unedited, undeleted, unreacted. A deleted message keeps its row
  with empty `content`.

  Sender and chat names come from `contacts`, falling back to `chats`. A store
  written by a bridge that predates that table still works — senders just show
  as JIDs until the bridge is restarted, which creates and backfills it.
- Everything else (sending messages/media, downloading media, listing group
  members) goes over HTTP to the bridge's REST API, not to any local file.

## Access control

Each server instance is scoped to a fixed set of chats by the JSON file named
in `WHATSAPP_MCP_ALLOWLIST`, so several MCP clients can share one bridge and
WhatsApp account while each sees only its own chats. There is no default: the
server exits at startup if the variable is unset, the file is unreadable, or
either scope is missing.

```json
{
  "name": "family",
  "allowRead": [
    { "id": "447700900000", "name": "Mum" },
    { "id": "120363000000000000@g.us", "name": "Family group" }
  ],
  "allowWrite": [
    { "id": "120363000000000000@g.us", "name": "Family group" }
  ]
}
```

- `id` is a phone number (country code, no `+`), an individual JID
  (`…@s.whatsapp.net` or `…@lid`) or a group JID (`…@g.us`). `name` is for
  whoever maintains the file and plays no part in the checks.
- `allowRead` covers every read tool, `download_media` and
  `list_group_members`. Unlisted chats are filtered out of listings and
  searches; naming one explicitly raises an error.
- `allowWrite` covers `send_message`, `send_file`, `send_audio_message`,
  `send_reaction`, `edit_message` and `delete_message`. `[]` makes the
  instance read-only; omitting the key fails closed.
- `"*"` in place of the list allows every chat for that scope.
- A person listed by phone number also matches their `@lid` chat once the
  bridge has recorded the pairing in `jid_aliases`, and the other way round.
  Until then, list both forms.

Keep the files outside the repository: they hold phone numbers. The
`*.allowlist.json` ignore rule is a backstop, not the plan.
`allowlist.example.json` is a template.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `WHATSAPP_MCP_ALLOWLIST` | none (required) | Path to the access-control file described above. |
| `WHATSAPP_MCP_DB_PATH` | `../whatsapp-bridge/store/messages.db` (relative to this dir) | Overrides the path to the bridge's message store, opened read-only. |
| `WHATSAPP_API_BASE_URL` | `http://localhost:8080/api` | Overrides the bridge's REST API base URL. |
| `WHATSAPP_MCP_ALLOWED_MEDIA_DIRS` | unset (no restriction) | Allowlist of directories that outbound media files (for `send_file`/`send_audio_message`) must live under, separated by `os.pathsep` (`:` on Unix, `;` on Windows). Paths are resolved with `realpath` before comparison, but the caller's original path is what gets sent. Unset means any existing file is allowed. |
| `WHATSAPP_MCP_SEND_RATE_LIMIT` | `30` | Max outbound sends (across `send_message`, `send_file`, `send_audio_message`, `send_reaction`, `edit_message`, `delete_message`) per rolling minute. `0` or a negative value disables the limiter. |
| `WHATSAPP_MCP_LOG_LEVEL` | `INFO` | Log level for the server's stderr logging. stdout carries only JSON-RPC — nothing is ever printed to stdout. |

## FFmpeg

FFmpeg is optional and only used by `send_audio_message` to convert non-Opus
audio files to `.ogg` Opus (the format WhatsApp requires for playable voice
messages). Without FFmpeg installed, `send_audio_message` will fail on
non-Opus input; raw audio can still be sent via `send_file` (it just won't
render as a playable voice message).

## Module layout

- `main.py` — entrypoint; imports `server` and the tool modules, runs the
  stdio transport.
- `server.py` — the shared `MCPServer` instance.
- `access_control.py` — loads the allowlist; `read_filter` for SQL,
  `check_readable`/`check_writable` for single chats and bridge calls.
- `config.py` — env var reads, logging setup.
- `tools_read.py` / `tools_send.py` — the `@mcp.tool()` wrappers.
- `models.py` — dataclasses for messages/chats/contacts.
- `db.py`, `db_names.py`, `db_chats.py`, `db_contacts.py`, `db_messages.py` —
  SQLite read layer. `db_names.py` owns every SQL fragment that turns stored
  columns into a display name, so one rule about what counts as a name applies
  to all queries. `db_contacts.py` holds the people-oriented lookups (finding a
  contact, the chats they appear in) and builds on `db_chats.chat_query`.
- `api_client.py` — REST calls to the bridge (send/download/group members),
  rate limiting, media-path validation.
- `api_actions.py` — react/edit/delete calls, on api_client's limiter and transport.
- `db_reactions.py` — bulk fetch of a page's reactions.
- `whatsapp.py` — thin façade re-exporting the public functions for backward
  compatible imports.
- `audio.py` — FFmpeg conversion helper.

Two names that used to live in `whatsapp.py` are gone on purpose, since both
are now env-overridable and therefore read at call time, not import time:
`MESSAGES_DB_PATH` → `config.messages_db_path()` (re-exported as
`whatsapp.messages_db_path`) and `WHATSAPP_API_BASE_URL` →
`config.api_base_url()` (re-exported as `whatsapp.api_base_url`).
`format_messages_list` is also gone: the read tools now return structured
dicts, so only `format_message` (used by `get_last_interaction`) remains.

Tests use pytest against a throwaway store and a stubbed bridge:

```bash
uv run pytest
```
