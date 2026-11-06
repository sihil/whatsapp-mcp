"""Per-instance chat/contact allowlist, with separate read and write scopes.

Each MCP server instance is scoped to a fixed set of WhatsApp chats via a JSON
config file. This lets multiple MCP servers share one WhatsApp bridge/account
while each only exposing a specific group of chats to its own client - and
lets an instance read a broad set of chats while only sending to a narrower
one.

The config path is read from the WHATSAPP_MCP_ALLOWLIST environment variable.
There is no "unset means allow everything" fallback: if the variable is
missing, unreadable, or malformed, the server refuses to start rather than
silently exposing every chat.

Config format (JSON):

    {
      "name": "family",
      "allowRead": [
        {"id": "447700900000", "name": "Mum"},                 // phone number -> personal chat
        {"id": "447700900000@s.whatsapp.net", "name": "Mum"},  // explicit individual JID
        {"id": "120363000000000000@g.us", "name": "Family group"}
      ],
      "allowWrite": [
        {"id": "120363000000000000@g.us", "name": "Family group"}
      ]
    }

Group JIDs are opaque numbers, so each entry carries a human-readable "name"
purely for maintainers editing this file - it plays no role in access checks,
which are keyed on "id" alone. "name" is optional but strongly recommended.

The bare string "*" (no object wrapper) allows every chat for that scope -
only use this for an explicit, intentionally-unrestricted instance.

"allowRead" must be non-empty. "allowWrite" may be an explicit empty list
("allowWrite": []) to mean a read-only instance with no send capability at
all; omitting the key entirely still fails closed, same as a missing
"allowRead".
"""

import json
import os
import sqlite3
import sys
from typing import Any, Optional

import db

_ENV_VAR = "WHATSAPP_MCP_ALLOWLIST"
_DEFAULT_SERVER = "s.whatsapp.net"


def _normalize(entry: str) -> str:
    entry = entry.strip()
    if entry == "*":
        return "*"
    if "@" not in entry:
        return f"{entry}@{_DEFAULT_SERVER}"
    return entry


def _entry_id(entry: Any, path: str, key: str) -> str:
    if entry == "*":
        return "*"
    if not isinstance(entry, dict) or not isinstance(entry.get("id"), str):
        sys.exit(
            f"Allowlist config at {path} has a malformed '{key}' entry: "
            f"{entry!r} - expected \"*\" or an object with an \"id\" string"
        )
    return _normalize(entry["id"])


def _load_scope(config: dict, path: str, key: str, allow_empty: bool = False) -> frozenset[str]:
    entries = config.get(key)
    if not isinstance(entries, list) or (not entries and not allow_empty):
        sys.exit(f"Allowlist config at {path} has no non-empty '{key}' list")
    return frozenset(_entry_id(e, path, key) for e in entries)


def _load() -> tuple[str, frozenset[str], frozenset[str]]:
    path = os.environ.get(_ENV_VAR)
    if not path:
        sys.exit(
            f"{_ENV_VAR} is not set. Refusing to start with an unrestricted "
            f"allowlist - point it at a JSON config listing the chats this "
            f"MCP server instance may access."
        )

    try:
        with open(path, "r") as f:
            config = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        sys.exit(f"Failed to load allowlist config from {path}: {e}")

    read_jids = _load_scope(config, path, "allowRead")
    write_jids = _load_scope(config, path, "allowWrite", allow_empty=True)

    name = config.get("name", os.path.basename(path))
    return name, read_jids, write_jids


INSTANCE_NAME, _ALLOWED_READ_JIDS, _ALLOWED_WRITE_JIDS = _load()
_ALLOW_READ_ALL = "*" in _ALLOWED_READ_JIDS
_ALLOW_WRITE_ALL = "*" in _ALLOWED_WRITE_JIDS


def _expand_aliases(cursor: Optional[sqlite3.Cursor], jids: frozenset[str]) -> frozenset[str]:
    """Add every spelling the bridge has recorded as the same chat.

    The bridge merges an @lid key into its phone-number key once it learns the
    mapping and leaves the old key in jid_aliases, so an entry written in
    either form has to match rows stored under the other.
    """
    if cursor is None or not jids:
        return jids
    slots = ",".join("?" for _ in jids)
    try:
        cursor.execute(
            f"SELECT old_jid, new_jid FROM jid_aliases "
            f"WHERE old_jid IN ({slots}) OR new_jid IN ({slots})",
            (*jids, *jids),
        )
    except sqlite3.OperationalError:
        return jids
    expanded = set(jids)
    for old_jid, new_jid in cursor.fetchall():
        expanded.update((old_jid, new_jid))
    return frozenset(expanded)


def _with_cursor(check, cursor: Optional[sqlite3.Cursor]):
    if cursor is not None:
        return check(cursor)
    try:
        conn = db.connect()
    except RuntimeError:
        return check(None)
    try:
        return check(conn.cursor())
    finally:
        conn.close()


def read_filter(cursor: sqlite3.Cursor, column: str) -> tuple[str, list[str]]:
    """SQL fragment + params restricting `column` to readable chat JIDs."""
    if _ALLOW_READ_ALL:
        return "1=1", []
    jids = sorted(_expand_aliases(cursor, _ALLOWED_READ_JIDS))
    return f"{column} IN ({','.join('?' for _ in jids)})", jids


def check_readable(jid: str, cursor: Optional[sqlite3.Cursor] = None) -> None:
    """Raise PermissionError unless the chat `jid` may be read."""
    if _ALLOW_READ_ALL:
        return
    if _normalize(jid) not in _with_cursor(
        lambda c: _expand_aliases(c, _ALLOWED_READ_JIDS), cursor
    ):
        raise PermissionError(
            f"{jid} is not in the read allowlist of the '{INSTANCE_NAME}' WhatsApp MCP server"
        )


def check_writable(recipient: str, cursor: Optional[sqlite3.Cursor] = None) -> None:
    """Raise PermissionError unless `recipient` (phone number or JID) may be
    sent to or have its messages reacted to, edited or deleted."""
    if _ALLOW_WRITE_ALL:
        return
    if _normalize(recipient) not in _with_cursor(
        lambda c: _expand_aliases(c, _ALLOWED_WRITE_JIDS), cursor
    ):
        raise PermissionError(
            f"{recipient} is not in the write allowlist of the '{INSTANCE_NAME}' WhatsApp MCP server"
        )
