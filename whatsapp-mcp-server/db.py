"""Read-only access to the bridge's SQLite message store: connection handling
and JID/alias resolution shared by the query modules."""
import os
import sqlite3
from datetime import datetime
from typing import Any, Dict, Iterable, List, Optional, Sequence
from urllib.parse import quote

from config import messages_db_path
from models import Chat, Message

MESSAGE_COLUMNS = (
    "messages.id, messages.chat_jid, chats.name, messages.sender, "
    "messages.content, messages.timestamp, messages.is_from_me, messages.media_type"
)

# Stored timestamps carry the offset Go wrote them with, so they can only be
# compared against a caller-supplied instant once both sides are resolved to
# UTC. SQLite's datetime() does that for any offset-bearing ISO-8601 string.
DATE_EXPR = "datetime(messages.timestamp)"


def connect() -> sqlite3.Connection:
    """Open the messages DB, read-only where possible.

    The bridge (see whatsapp-bridge/main.go NewMessageStore) uses SQLite's
    default rollback-journal mode. A hot journal — left behind when the bridge
    crashes mid-write — has to be replayed before the database can be read at
    all, and SQLite can only do that through a writable connection: a mode=ro
    open would fail every read with SQLITE_READONLY_ROLLBACK. So fall back to
    a read-write open for exactly that case, and stay read-only otherwise.
    """
    path = messages_db_path()
    if not os.path.isfile(path):
        raise RuntimeError(
            f"messages.db not found at {path} — start the bridge first: "
            "cd whatsapp-bridge && go run ."
        )
    journal = f"{path}-journal"
    if os.path.isfile(journal) and os.path.getsize(journal) > 0:
        return sqlite3.connect(path)
    return sqlite3.connect(f"file:{quote(path)}?mode=ro", uri=True)


def escape_like(term: str, escape_char: str = "\\") -> str:
    """Escape a user-supplied fragment for safe use inside a LIKE pattern.
    Pair with `LIKE ? ESCAPE '\\'` in the SQL."""
    return (
        term.replace(escape_char, escape_char * 2)
        .replace("%", f"{escape_char}%")
        .replace("_", f"{escape_char}_")
    )


def canonical_jid(identifier: str) -> str:
    """Normalize a bare phone number to the canonical JID form the bridge
    stores (full user@server strings, phone-number namespace preferred)."""
    if "@" in identifier:
        return identifier
    return f"{identifier}@s.whatsapp.net"


def resolve_chat_jid(cursor, jid: Optional[str]) -> Optional[str]:
    """Follow a stale chat key (e.g. an @lid key the bridge later merged into
    its phone-number form) to its current value via the bridge-maintained
    jid_aliases table."""
    if not jid:
        return jid
    try:
        cursor.execute("SELECT new_jid FROM jid_aliases WHERE old_jid = ?", (jid,))
        row = cursor.fetchone()
        return row[0] if row else jid
    except sqlite3.OperationalError:
        # Table absent: DB written by a bridge from before aliases existed.
        return jid


def alias_candidates(cursor: sqlite3.Cursor, jid: str) -> List[str]:
    """Every spelling that may key rows for `jid`: the value itself, the key it
    was merged into, and any stale keys merged into those."""
    candidates = {jid}
    resolved = resolve_chat_jid(cursor, jid)
    if resolved:
        candidates.add(resolved)
    try:
        for target in list(candidates):
            cursor.execute("SELECT old_jid FROM jid_aliases WHERE new_jid = ?", (target,))
            candidates.update(row[0] for row in cursor.fetchall())
    except sqlite3.OperationalError:
        pass
    return sorted(candidates)


def placeholders(values: Sequence[Any]) -> str:
    return ", ".join("?" * len(values))


def parse_date_param(value: str, field: str) -> str:
    """Validate an ISO-8601 argument and return it in a form SQLite's
    `datetime()` can normalize. Never bind datetime objects: their sqlite3
    adapters are deprecated in 3.12.

    Always carries an explicit offset. The bridge stores Go time.Time values
    rendered in local time ("2025-08-04 14:23:11-07:00"), so comparing raw
    strings only works when the argument happens to carry the same offset —
    "2025-08-04T20:00:00Z" would compare character-by-character as *earlier*
    than a row that is genuinely after it. Both sides go through
    `DATE_EXPR` instead, which resolves each to UTC before comparing. A
    naive argument is taken as local time, which is how a user means it.
    """
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        raise ValueError(
            f"Invalid date format for '{field}': {value}. Please use ISO-8601 format."
        )
    # astimezone() attaches the local zone to a naive value and converts an
    # aware one; either way the result is unambiguous.
    return parsed.astimezone().isoformat(sep=" ")


def resolve_sender_names(
    cursor: sqlite3.Cursor,
    senders: Iterable[str],
    cache: Optional[Dict[str, Optional[str]]] = None,
) -> Dict[str, str]:
    """Map sender JIDs to display names in one bulk query plus one fallback
    lookup per still-unresolved sender, rather than one connection per message.
    Pass a `cache` dict (misses included) to resolve each JID at most once
    across several batches, e.g. a listing and its per-match context windows."""
    unique = [sender for sender in dict.fromkeys(senders) if sender]
    if not unique:
        return {}

    names: Dict[str, str] = {}
    pending: List[str] = []
    for sender in unique:
        if cache is not None and sender in cache:
            cached = cache[sender]
            if cached:
                names[sender] = cached
        else:
            pending.append(sender)
    if not pending:
        return names

    cursor.execute(
        f"SELECT jid, name FROM chats WHERE jid IN ({placeholders(pending)})", tuple(pending)
    )
    for jid, name in cursor.fetchall():
        if name:
            names[jid] = name

    for sender in pending:
        if sender not in names:
            phone_part = sender.split("@")[0]
            # Anchor on the user part and exclude groups: a bare substring
            # match would happily label a sender with a group's name, or with
            # another contact whose number merely contains these digits.
            cursor.execute(
                "SELECT name FROM chats WHERE jid LIKE ? ESCAPE '\\' "
                "AND jid NOT LIKE '%@g.us' AND name IS NOT NULL AND name != '' LIMIT 1",
                (f"{escape_like(phone_part)}@%",),
            )
            row = cursor.fetchone()
            if row and row[0]:
                names[sender] = row[0]
        if cache is not None:
            cache[sender] = names.get(sender)
    return names


def get_sender_name(sender_jid: str, cursor: Optional[sqlite3.Cursor] = None) -> str:
    """Display name for a sender JID, falling back to the JID itself."""
    if cursor is not None:
        return resolve_sender_names(cursor, [sender_jid]).get(sender_jid, sender_jid)
    conn = connect()
    try:
        return resolve_sender_names(conn.cursor(), [sender_jid]).get(sender_jid, sender_jid)
    finally:
        conn.close()


def message_from_row(row: Sequence[Any], sender_names: Optional[Dict[str, str]] = None) -> Message:
    """Build a Message from a row selected with MESSAGE_COLUMNS."""
    sender = row[3]
    is_from_me = bool(row[6])
    if is_from_me:
        sender_name = "Me"
    else:
        sender_name = (sender_names or {}).get(sender, sender)
    return Message(
        id=row[0],
        chat_jid=row[1],
        chat_name=row[2],
        sender=sender,
        content=row[4],
        timestamp=datetime.fromisoformat(row[5]),
        is_from_me=is_from_me,
        media_type=row[7],
        sender_name=sender_name,
    )


def messages_from_rows(
    cursor: sqlite3.Cursor,
    rows: Sequence[Sequence[Any]],
    name_cache: Optional[Dict[str, Optional[str]]] = None,
) -> List[Message]:
    names = resolve_sender_names(cursor, [row[3] for row in rows if not row[6]], name_cache)
    return [message_from_row(row, names) for row in rows]


def chat_from_row(row: Sequence[Any]) -> Chat:
    """Build a Chat from a row selected with CHAT_COLUMNS (see db_chats)."""
    return Chat(
        jid=row[0],
        name=row[1],
        last_message_time=datetime.fromisoformat(row[2]) if row[2] else None,
        last_message=row[3],
        last_sender=row[4],
        last_is_from_me=None if row[5] is None else bool(row[5]),
    )
