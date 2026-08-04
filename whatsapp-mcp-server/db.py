"""Read-only access to the bridge's SQLite message store: connection handling
and JID/alias resolution shared by the query modules."""
import os
import sqlite3
from typing import Optional
from urllib.parse import quote

from config import MESSAGES_DB_PATH


def connect() -> sqlite3.Connection:
    """Open the messages DB, read-only where possible.

    The bridge (see whatsapp-bridge/main.go NewMessageStore) uses SQLite's
    default rollback-journal mode. A hot journal — left behind when the bridge
    crashes mid-write — has to be replayed before the database can be read at
    all, and SQLite can only do that through a writable connection: a mode=ro
    open would fail every read with SQLITE_READONLY_ROLLBACK. So fall back to
    a read-write open for exactly that case, and stay read-only otherwise.
    """
    path = MESSAGES_DB_PATH
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
