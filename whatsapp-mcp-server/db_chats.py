"""Chat queries against the bridge's message store. Contact-oriented lookups
live in db_contacts.py, which builds on the query helpers here."""
from typing import Any, Dict, List, Optional

from db import (
    canonical_jid,
    chat_from_row,
    connect,
    escape_like,
    get_sender_name,
    resolve_chat_jid,
)
from db_names import chat_name_expr, contacts_available, contacts_join
from models import Message

_LAST_MESSAGE_JOIN = (
    "LEFT JOIN messages m ON c.jid = m.chat_jid AND c.last_message_time = m.timestamp"
)


def chat_query(
    with_contacts: bool,
    include_last_message: bool,
    where: str = "",
    suffix: str = "",
) -> str:
    """Build a chat query, optionally joined to the message at
    `last_message_time`. Shared with db_contacts.py.

    Always groups by chat. The join matches on timestamp equality, and
    WhatsApp timestamps are second-granularity, so a chat whose last second
    holds more than one message would otherwise be emitted once per message —
    duplicating it in the results and consuming two LIMIT slots, which pushes
    another chat off the page entirely (and off every later page too, since
    OFFSET then counts the duplicates).
    """
    last_message = (
        "m.content, m.sender, m.is_from_me" if include_last_message else "NULL, NULL, NULL"
    )

    parts = [
        f"SELECT c.jid, {chat_name_expr(with_contacts)}, c.last_message_time, "
        f"{last_message} FROM chats c"
    ]
    if with_contacts:
        parts.append(contacts_join(with_contacts))
    if include_last_message:
        parts.append(_LAST_MESSAGE_JOIN)
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

        with_contacts = contacts_available(cursor)
        name_expr = chat_name_expr(with_contacts)
        if query:
            escaped_query = escape_like(query)
            where = (
                f"WHERE (LOWER({name_expr}) LIKE LOWER(?) ESCAPE '\\' "
                "OR c.jid LIKE ? ESCAPE '\\')"
            )
            params.extend([f"%{escaped_query}%", f"%{escaped_query}%"])

        order_by = "c.last_message_time DESC" if sort_by == "last_active" else name_expr
        params.extend([limit, page * limit])

        cursor.execute(
            chat_query(
                with_contacts, include_last_message, where,
                f"ORDER BY {order_by} LIMIT ? OFFSET ?",
            ),
            tuple(params),
        )
        return [chat_from_row(row).to_dict() for row in cursor.fetchall()]
    finally:
        conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Dict[str, Any]:
    """Get chat metadata by JID."""
    conn = connect()
    try:
        cursor = conn.cursor()
        resolved_jid = resolve_chat_jid(cursor, canonical_jid(chat_jid))
        cursor.execute(
            chat_query(contacts_available(cursor), include_last_message, "WHERE c.jid = ?"),
            (resolved_jid,),
        )
        row = cursor.fetchone()
        if not row:
            raise ValueError(f"No chat found with JID {chat_jid}")
        return chat_from_row(row).to_dict()
    finally:
        conn.close()
