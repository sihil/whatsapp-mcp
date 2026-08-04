"""Chat and contact queries against the bridge's message store."""
from datetime import datetime
from typing import Any, List, Optional

from db import canonical_jid, connect, get_sender_name, resolve_chat_jid
from models import Chat, Contact, Message

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


def format_message(message: Message, show_chat_info: bool = True) -> str:
    """Render a single message as a human-readable line."""
    output = ""

    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "

    # Always surface the message ID: it is the handle for reply_to and download_media
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    else:
        content_prefix = f"[Message ID: {message.id}] "

    sender_name = "Me" if message.is_from_me else get_sender_name(message.sender)
    output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    return output


def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> str:
    output = ""
    if not messages:
        output += "No messages to display."
        return output

    for message in messages:
        output += format_message(message, show_chat_info)
    return output


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    conn = connect()
    try:
        cursor = conn.cursor()
        where = ""
        params: List[Any] = []

        if query:
            where = "WHERE (LOWER(c.name) LIKE LOWER(?) OR c.jid LIKE ?)"
            params.extend([f"%{query}%", f"%{query}%"])

        order_by = "c.last_message_time DESC" if sort_by == "last_active" else "c.name"
        params.extend([limit, page * limit])

        cursor.execute(
            _chat_query(
                include_last_message, where, f"ORDER BY {order_by} LIMIT ? OFFSET ?"
            ),
            tuple(params),
        )
        chats = cursor.fetchall()

        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)

        return result

    finally:
        conn.close()


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number."""
    conn = connect()
    try:
        cursor = conn.cursor()

        # Split query into characters to support partial matching
        search_pattern = '%' + query + '%'

        cursor.execute("""
            SELECT DISTINCT
                jid,
                name
            FROM chats
            WHERE
                (LOWER(name) LIKE LOWER(?) OR LOWER(jid) LIKE LOWER(?))
                AND jid NOT LIKE '%@g.us'
            ORDER BY name, jid
            LIMIT 50
        """, (search_pattern, search_pattern))

        contacts = cursor.fetchall()

        result = []
        for contact_data in contacts:
            contact = Contact(
                phone_number=contact_data[0].split('@')[0],
                name=contact_data[1],
                jid=contact_data[0]
            )
            result.append(contact)

        return result

    finally:
        conn.close()


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact.

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    conn = connect()
    try:
        cursor = conn.cursor()

        resolved_jid = resolve_chat_jid(cursor, canonical_jid(jid))
        cursor.execute(
            _chat_query(
                True,
                "WHERE c.jid IN (?, ?) "
                "OR c.jid IN (SELECT chat_jid FROM messages WHERE sender IN (?, ?))",
                "ORDER BY c.last_message_time DESC LIMIT ? OFFSET ?",
            ),
            (jid, resolved_jid, jid, resolved_jid, limit, page * limit),
        )

        chats = cursor.fetchall()

        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)

        return result

    finally:
        conn.close()


def get_last_interaction(jid: str) -> Optional[str]:
    """Get most recent message involving the contact."""
    conn = connect()
    try:
        cursor = conn.cursor()

        resolved_jid = resolve_chat_jid(cursor, canonical_jid(jid))
        cursor.execute("""
            SELECT
                m.timestamp,
                m.sender,
                c.name,
                m.content,
                m.is_from_me,
                c.jid,
                m.id,
                m.media_type
            FROM messages m
            JOIN chats c ON m.chat_jid = c.jid
            WHERE m.sender IN (?, ?) OR c.jid IN (?, ?)
            ORDER BY m.timestamp DESC
            LIMIT 1
        """, (jid, resolved_jid, jid, resolved_jid))

        msg_data = cursor.fetchone()

        if not msg_data:
            return None

        message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[7]
        )

        return format_message(message)

    finally:
        conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    conn = connect()
    try:
        cursor = conn.cursor()

        resolved_jid = resolve_chat_jid(cursor, canonical_jid(chat_jid))
        cursor.execute(_chat_query(include_last_message, "WHERE c.jid = ?"), (resolved_jid,))
        chat_data = cursor.fetchone()

        if not chat_data:
            return None

        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )

    finally:
        conn.close()


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number."""
    conn = connect()
    try:
        cursor = conn.cursor()

        cursor.execute(
            _chat_query(
                True, "WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us'", "LIMIT 1"
            ),
            (f"%{sender_phone_number}%",),
        )

        chat_data = cursor.fetchone()

        if not chat_data:
            return None

        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )

    finally:
        conn.close()
