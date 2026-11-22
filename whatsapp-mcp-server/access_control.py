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

An entry for a community with "include": "community" also covers every group
linked to it, including groups linked after the server started: the bridge
records the links in group_links and every check reads them afresh. Without
"include" a community entry covers only the community itself.

    {"id": "120363000000000100@g.us", "name": "Edinburgh", "include": "community"}

"memberDMs": true (off unless set) also makes readable the DM with anyone
who is in a group this instance can read and in none of the account's other
groups, and is not saved in the address book - for an instance scoped to an
organisation's communities, where people message one-to-one as well as in
the groups, without taking in friends who happen to be in them too. The
bridge records group members in group_participants; every check reads them
afresh.

"denyRead" lists chats that are never readable, whatever else admits them:
the way to keep out someone memberDMs lets in.

    "memberDMs": true,
    "denyRead": [{"id": "447700900001", "name": "A friend"}]

The bare string "*" (no object wrapper) allows every chat for that scope -
only use this for an explicit, intentionally-unrestricted instance.

"allowRead" must be non-empty. "allowWrite" may be an explicit empty list
("allowWrite": []) to mean a read-only instance with no send capability at
all; omitting the key entirely still fails closed, same as a missing
"allowRead".

Edits to the file apply to the next check without a restart. An edit that
leaves the file missing, unreadable or invalid denies every chat until it is
fixed, rather than keeping the previous config.
"""

import json
import logging
import os
import sqlite3
import sys
from typing import Any, NamedTuple, Optional

import db
from db_reactions import own_jid

logger = logging.getLogger(__name__)

_ENV_VAR = "WHATSAPP_MCP_ALLOWLIST"
_DEFAULT_SERVER = "s.whatsapp.net"


def _normalize(entry: str) -> str:
    entry = entry.strip()
    if entry == "*":
        return "*"
    if "@" not in entry:
        return f"{entry}@{_DEFAULT_SERVER}"
    return entry


class Scope(NamedTuple):
    jids: frozenset[str]
    communities: frozenset[str]  # each also covers the groups linked to it


class Config(NamedTuple):
    name: str
    read: Scope
    write: Scope
    member_dms: bool = False
    deny: frozenset[str] = frozenset()


class AllowlistError(Exception):
    pass


def _entry_id(entry: Any, path: str, key: str) -> tuple[str, bool]:
    """An entry's normalised id, and whether it covers a whole community."""
    if entry == "*":
        return "*", False
    if not isinstance(entry, dict) or not isinstance(entry.get("id"), str):
        raise AllowlistError(
            f"Allowlist config at {path} has a malformed '{key}' entry: "
            f"{entry!r} - expected \"*\" or an object with an \"id\" string"
        )
    include = entry.get("include")
    if include not in (None, "community"):
        raise AllowlistError(
            f"Allowlist config at {path} has a '{key}' entry with unknown "
            f"include {include!r} - the only option is \"community\""
        )
    return _normalize(entry["id"]), include == "community"


def _load_scope(config: dict, path: str, key: str, allow_empty: bool = False) -> Scope:
    entries = config.get(key)
    if not isinstance(entries, list) or (not entries and not allow_empty):
        raise AllowlistError(f"Allowlist config at {path} has no non-empty '{key}' list")
    ids = [_entry_id(e, path, key) for e in entries]
    return Scope(
        jids=frozenset(jid for jid, community in ids if not community),
        communities=frozenset(jid for jid, community in ids if community),
    )


def _parse(path: str) -> Config:
    try:
        with open(path, "r") as f:
            config = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        raise AllowlistError(f"Failed to load allowlist config from {path}: {e}") from e
    if not isinstance(config, dict):
        raise AllowlistError(f"Allowlist config at {path} is not a JSON object")
    member_dms = config.get("memberDMs", False)
    if not isinstance(member_dms, bool):
        raise AllowlistError(f"Allowlist config at {path} has a non-boolean 'memberDMs': {member_dms!r}")
    deny = config.get("denyRead", [])
    if not isinstance(deny, list):
        raise AllowlistError(f"Allowlist config at {path} has a 'denyRead' that is not a list")
    deny_ids = [_entry_id(e, path, "denyRead") for e in deny]
    if any(jid == "*" or community for jid, community in deny_ids):
        raise AllowlistError(f"Allowlist config at {path} has a 'denyRead' entry that is \"*\" or a community")
    return Config(
        name=config.get("name", os.path.basename(path)),
        read=_load_scope(config, path, "allowRead"),
        write=_load_scope(config, path, "allowWrite", allow_empty=True),
        member_dms=member_dms,
        deny=frozenset(jid for jid, _ in deny_ids),
    )


def _path() -> str:
    path = os.environ.get(_ENV_VAR)
    if not path:
        sys.exit(
            f"{_ENV_VAR} is not set. Refusing to start with an unrestricted "
            f"allowlist - point it at a JSON config listing the chats this "
            f"MCP server instance may access."
        )
    return path


def _load() -> Config:
    """The config at startup, where any problem stops the server."""
    try:
        return _parse(_path())
    except AllowlistError as e:
        sys.exit(str(e))


def _signature(path: str) -> tuple[int, int, int]:
    st = os.stat(path)
    return st.st_mtime_ns, st.st_size, st.st_ino


class _State(NamedTuple):
    signature: tuple[int, int, int]
    config: Optional[Config]
    error: Optional[str]


_PATH = _path()
_state = _State(_signature(_PATH), _load(), None)


def _current() -> Config:
    """The config as the file stands now, re-read whenever the file changes.

    Raises PermissionError while the file is missing or invalid.
    """
    global _state
    try:
        signature = _signature(_PATH)
    except OSError as e:
        raise PermissionError(
            f"Cannot read the allowlist config at {_PATH} ({e}); every chat is "
            "denied until it is restored"
        ) from e
    state = _state
    if signature != state.signature:
        try:
            state = _State(signature, _parse(_PATH), None)
            logger.info("Reloaded allowlist config from %s", _PATH)
        except AllowlistError as e:
            state = _State(signature, None, str(e))
            logger.error("%s; denying every chat until it is fixed", e)
        _state = state
    if state.config is None:
        raise PermissionError(f"{state.error}; every chat is denied until it is fixed")
    return state.config


def instance_name() -> str:
    return _current().name


def _expand_communities(cursor: Optional[sqlite3.Cursor], scope: Scope) -> frozenset[str]:
    """The scope's plain entries, its communities, and every group currently
    linked to one of those communities."""
    jids = scope.jids | scope.communities
    if cursor is None or not scope.communities:
        return jids
    slots = ",".join("?" for _ in scope.communities)
    try:
        cursor.execute(
            f"SELECT group_jid FROM group_links WHERE parent_jid IN ({slots})",
            tuple(scope.communities),
        )
    except sqlite3.OperationalError:
        return jids
    return jids | {row[0] for row in cursor.fetchall()}


def _expand(cursor: Optional[sqlite3.Cursor], scope: Scope) -> frozenset[str]:
    return _expand_aliases(cursor, _expand_communities(cursor, scope))


def _is_dm(jid: str) -> bool:
    return jid.endswith("@s.whatsapp.net") or jid.endswith("@lid")


def member_dm_candidates(cursor: sqlite3.Cursor, readable: frozenset[str]) -> dict[str, list[str]]:
    """Everyone in a readable group, other than the account itself, with the
    groups outside `readable` they are also in. Empty on a store without
    group_participants."""
    groups = sorted(j for j in readable if j.endswith("@g.us"))
    if not groups:
        return {}
    slots = ",".join("?" for _ in groups)
    try:
        cursor.execute(
            f"""SELECT inside.participant, outside.group_jid
                FROM (SELECT DISTINCT participant FROM group_participants
                      WHERE group_jid IN ({slots})) inside
                LEFT JOIN group_participants outside
                  ON outside.participant = inside.participant
                 AND outside.group_jid NOT IN ({slots})""",
            (*groups, *groups),
        )
    except sqlite3.OperationalError:
        return {}
    rows = cursor.fetchall()
    own = own_jid(cursor)
    found: dict[str, list[str]] = {}
    for participant, outside in rows:
        if participant == own or not _is_dm(participant):
            continue
        found.setdefault(participant, [])
        if outside:
            found[participant].append(outside)
    return found


def saved_contacts(cursor: sqlite3.Cursor, jids) -> frozenset[str]:
    jids = list(jids)
    if not jids:
        return frozenset()
    try:
        cursor.execute(
            f"SELECT jid FROM contacts WHERE TRIM(full_name) <> '' "
            f"AND jid IN ({','.join('?' for _ in jids)})",
            jids,
        )
    except sqlite3.OperationalError:
        return frozenset()
    return frozenset(row[0] for row in cursor.fetchall())


def _member_dms(cursor: sqlite3.Cursor, readable: frozenset[str]) -> frozenset[str]:
    candidates = member_dm_candidates(cursor, readable)
    only_inside = [jid for jid, outside in candidates.items() if not outside]
    return frozenset(only_inside) - saved_contacts(cursor, only_inside)


def _readable(cursor: Optional[sqlite3.Cursor], config: Config) -> frozenset[str]:
    jids = _expand(cursor, config.read)
    if config.member_dms and cursor is not None:
        jids |= _expand_aliases(cursor, _member_dms(cursor, jids))
    return jids - _expand_aliases(cursor, config.deny)


def readable_jids(cursor: sqlite3.Cursor) -> Optional[frozenset[str]]:
    """Every chat JID that may be read, or None when every chat may."""
    config = _current()
    if "*" in config.read.jids and not config.deny:
        return None
    if "*" in config.read.jids:
        raise PermissionError("denyRead cannot be combined with allowRead \"*\"")
    return _readable(cursor, config)


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
    jids = readable_jids(cursor)
    if jids is None:
        return "1=1", []
    jids = sorted(jids)
    return f"{column} IN ({','.join('?' for _ in jids)})", jids


def check_readable(jid: str, cursor: Optional[sqlite3.Cursor] = None) -> None:
    """Raise PermissionError unless the chat `jid` may be read."""
    config = _current()
    if "*" in config.read.jids and not config.deny:
        return
    if _normalize(jid) not in _with_cursor(
        lambda c: readable_jids(c) if c is not None else _expand(None, config.read) - config.deny, cursor
    ):
        raise PermissionError(
            f"{jid} is not in the read allowlist of the '{config.name}' WhatsApp MCP server"
        )


def check_writable(recipient: str, cursor: Optional[sqlite3.Cursor] = None) -> None:
    """Raise PermissionError unless `recipient` (phone number or JID) may be
    sent to or have its messages reacted to, edited or deleted."""
    config = _current()
    if "*" in config.write.jids:
        return
    if _normalize(recipient) not in _with_cursor(
        lambda c: _expand(c, config.write), cursor
    ):
        raise PermissionError(
            f"{recipient} is not in the write allowlist of the '{config.name}' WhatsApp MCP server"
        )
