"""Chat and contact queries against the bridge's message store."""
from typing import Any, Dict, List, Optional

from db import (
    MESSAGE_COLUMNS,
    alias_candidates,
    canonical_jid,
    connect,
    escape_like,
    resolve_chat_jid,
    chat_from_row,
    get_sender_name,
    message_from_row,
    placeholders,
    resolve_sender_names,
)
from models import Contact, Message

_CHAT_WITH_LAST_MESSAGE = """
    SELECT c.jid, c.name, c.last_message_time, m.content, m.sender, m.is_from_me
    FROM chats c
    LEFT JOIN messages m ON c.jid = m.chat_jid AND c.last_message_time = m.timestamp
"""
_CHAT_ONLY = "SELECT c.jid, c.name, c.last_message_time, NULL, NULL, NULL FROM chats c"


def _chat_query(include_last_message: bool, where: str = "", suffix: str = "") -> str:
    """Build a chat query, optionally joined to the message at
    `last_message_time`.

    Always groups by chat. The join matches on timestamp equality, and
    WhatsApp timestamps are second-granularity, so a chat whose last second
    holds more than one message would otherwise be emitted once per message —
    duplicating it in the results and consuming two LIMIT slots, which pushes
    another chat off the page entirely (and off every later page too, since
    OFFSET then counts the duplicates).
    """
    parts = [_CHAT_WITH_LAST_MESSAGE if include_last_message else _CHAT_ONLY]
    if where:
        parts.append(where)
    parts.append("GROUP BY c.jid")
    if suffix:
        parts.append(suffix)
    return " ".join(parts)


def format_message(message: Message, show_chat_info: bool = True, cursor=None) -> str:
    """Render a single message as a human-readable line."""
    if show_chat_info and message.chat_name:
        output = f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output = f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "

    # Always surface the message ID: it is the handle for reply_to and download_media
    if message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    else:
        content_prefix = f"[Message ID: {message.id}] "

    if message.is_from_me:
        sender_name = "Me"
    else:
        sender_name = message.sender_name or get_sender_name(message.sender, cursor)
    return f"{output}From: {sender_name}: {content_prefix}{message.content}\n"


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Dict[str, Any]]:
    """Get chats matching the specified criteria."""
    conn = connect()
    try:
        cursor = conn.cursor()
        where = ""
        params: List[Any] = []

        if query:
            escaped_query = escape_like(query)
            where = "WHERE (LOWER(c.name) LIKE LOWER(?) ESCAPE '\\' OR c.jid LIKE ? ESCAPE '\\')"
            params.extend([f"%{escaped_query}%", f"%{escaped_query}%"])

        order_by = "c.last_message_time DESC" if sort_by == "last_active" else "c.name"
        params.extend([limit, page * limit])

        cursor.execute(
            _chat_query(
                include_last_message, where, f"ORDER BY {order_by} LIMIT ? OFFSET ?"
            ),
            tuple(params),
        )
        return [chat_from_row(row).to_dict() for row in cursor.fetchall()]
    finally:
        conn.close()


def search_contacts(query: str, limit: int = 50) -> List[Dict[str, Any]]:
    """Search contacts by name or phone number."""
    conn = connect()
    try:
        cursor = conn.cursor()
        search_pattern = '%' + escape_like(query) + '%'
        # The bridge writes '' (not NULL) for an unknown chat name, and ''
        # sorts before every real name — so ordering by name alone lets
        # nameless JID matches fill the whole limit and drop the named
        # contacts the caller was actually looking for. Named ones first.
        cursor.execute("""
            SELECT DISTINCT
                jid,
                name
            FROM chats
            WHERE
                (LOWER(name) LIKE LOWER(?) ESCAPE '\\' OR LOWER(jid) LIKE LOWER(?) ESCAPE '\\')
                AND jid NOT LIKE '%@g.us'
            ORDER BY (name IS NULL OR name = ''), name, jid
            LIMIT ?
        """, (search_pattern, search_pattern, limit))

        return [
            Contact(phone_number=row[0].split('@')[0], name=row[1], jid=row[0]).to_dict()
            for row in cursor.fetchall()
        ]
    finally:
        conn.close()


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all chats involving the contact, most recently active first.

    Args:
        jid: The contact's JID (or bare phone number) to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    conn = connect()
    try:
        cursor = conn.cursor()
        candidates = alias_candidates(cursor, canonical_jid(jid))
        slots = placeholders(candidates)
        cursor.execute(
            _chat_query(
                True,
                f"WHERE c.jid IN ({slots}) "
                f"OR c.jid IN (SELECT chat_jid FROM messages WHERE sender IN ({slots}))",
                "ORDER BY c.last_message_time DESC LIMIT ? OFFSET ?",
            ),
            tuple(candidates) * 2 + (limit, page * limit),
        )

        return [chat_from_row(row).to_dict() for row in cursor.fetchall()]
    finally:
        conn.close()


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact, as a formatted line."""
    conn = connect()
    try:
        cursor = conn.cursor()
        candidates = alias_candidates(cursor, canonical_jid(jid))
        slots = placeholders(candidates)
        cursor.execute(f"""
            SELECT {MESSAGE_COLUMNS}
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.sender IN ({slots}) OR chats.jid IN ({slots})
            ORDER BY messages.timestamp DESC
            LIMIT 1
        """, tuple(candidates) * 2)

        row = cursor.fetchone()
        if not row:
            raise ValueError(f"No messages found involving {jid}")

        names = resolve_sender_names(cursor, [row[3]] if not row[6] else [])
        return format_message(message_from_row(row, names), cursor=cursor)
    finally:
        conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Dict[str, Any]:
    """Get chat metadata by JID."""
    conn = connect()
    try:
        cursor = conn.cursor()
        resolved_jid = resolve_chat_jid(cursor, canonical_jid(chat_jid))
        cursor.execute(_chat_query(include_last_message, "WHERE c.jid = ?"), (resolved_jid,))
        row = cursor.fetchone()
        if not row:
            raise ValueError(f"No chat found with JID {chat_jid}")
        return chat_from_row(row).to_dict()
    finally:
        conn.close()


def _phone_digits(value: str) -> str:
    """Digits of the user part of a phone number or JID ("+1 (646) 555-9999"
    and "16465559999@s.whatsapp.net" both yield "16465559999")."""
    return "".join(char for char in value.split("@")[0] if char.isdigit())


def get_direct_chat_by_contact(sender_phone_number: str) -> Dict[str, Any]:
    """Get direct (non-group) chat metadata by contact phone number, matching
    the JID's user part exactly before falling back to a suffix match."""
    digits = _phone_digits(sender_phone_number or "")
    if not digits:
        raise ValueError(f"No digits in phone number: {sender_phone_number!r}")

    conn = connect()
    try:
        cursor = conn.cursor()
        canonical = f"{digits}@s.whatsapp.net"
        candidates = alias_candidates(cursor, canonical)
        cursor.execute(
            _chat_query(
                True,
                f"WHERE c.jid IN ({placeholders(candidates)}) AND c.jid NOT LIKE '%@g.us'",
            ),
            tuple(candidates),
        )
        rows = cursor.fetchall()

        if not rows:
            cursor.execute(
                _chat_query(
                    True,
                    "WHERE c.jid NOT LIKE '%@g.us' "
                    "AND substr(c.jid, 1, instr(c.jid, '@') - 1) LIKE ? ESCAPE '\\'",
                ),
                (f"%{escape_like(digits)}",),
            )
            rows = cursor.fetchall()
            matched_jids = sorted({row[0] for row in rows})
            if len(matched_jids) > 1:
                raise ValueError(
                    f"Multiple direct chats end with {digits}: {', '.join(matched_jids)}. "
                    "Pass the full JID to get_chat instead."
                )
            if matched_jids:
                # A suffix match can land on a stale key (an @lid chat the
                # bridge has since merged); follow it to the surviving row.
                merged_jid = resolve_chat_jid(cursor, matched_jids[0])
                if merged_jid != matched_jids[0]:
                    cursor.execute(_chat_query(True, "WHERE c.jid = ?"), (merged_jid,))
                    merged_rows = cursor.fetchall()
                    if merged_rows:
                        rows = merged_rows

        if not rows:
            raise ValueError(f"No direct chat found for {sender_phone_number}")

        resolved = resolve_chat_jid(cursor, canonical)
        row = next((candidate for candidate in rows if candidate[0] == resolved), rows[0])
        return chat_from_row(row).to_dict()
    finally:
        conn.close()
