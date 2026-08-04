"""Read-only access to the bridge's SQLite message store: connection handling
and JID/alias resolution shared by the query modules."""
import os
import sqlite3
from datetime import datetime
from typing import Any, Optional, Sequence
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


def get_sender_name(sender_jid: str) -> str:
    conn = connect()
    try:
        cursor = conn.cursor()

        # First try matching by exact JID
        cursor.execute("""
            SELECT name
            FROM chats
            WHERE jid = ?
            LIMIT 1
        """, (sender_jid,))

        result = cursor.fetchone()

        # If no result, try looking for the number within JIDs
        if not result:
            # Extract the phone number part if it's a JID
            if '@' in sender_jid:
                phone_part = sender_jid.split('@')[0]
            else:
                phone_part = sender_jid

            cursor.execute("""
                SELECT name
                FROM chats
                WHERE jid LIKE ? ESCAPE '\\'
                LIMIT 1
            """, (f"%{escape_like(phone_part)}%",))

            result = cursor.fetchone()

        if result and result[0]:
            return result[0]
        else:
            return sender_jid

    finally:
        conn.close()


def message_from_row(row: Sequence[Any]) -> Message:
    """Build a Message from a row selected with MESSAGE_COLUMNS."""
    return Message(
        id=row[0],
        chat_jid=row[1],
        chat_name=row[2],
        sender=row[3],
        content=row[4],
        timestamp=datetime.fromisoformat(row[5]),
        is_from_me=bool(row[6]),
        media_type=row[7],
    )


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
