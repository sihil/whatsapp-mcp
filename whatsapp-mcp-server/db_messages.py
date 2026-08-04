"""Message queries against the bridge's message store."""
from datetime import datetime
from typing import Optional

from db import DATE_EXPR, canonical_jid, connect, escape_like, parse_date_param, resolve_chat_jid
from db_chats import format_messages_list
from models import Message, MessageContext


def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> str:
    """Get messages matching the specified criteria with optional context."""
    conn = connect()
    try:
        cursor = conn.cursor()

        # Build base query
        query_parts = ["SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        where_clauses = []
        params = []

        # Add filters
        if after:
            where_clauses.append(f"{DATE_EXPR} > datetime(?)")
            params.append(parse_date_param(after, "after"))

        if before:
            where_clauses.append(f"{DATE_EXPR} < datetime(?)")
            params.append(parse_date_param(before, "before"))

        if sender_phone_number:
            # The sender column stores full canonical JIDs; also match the
            # raw value for rows the bridge has not migrated yet.
            where_clauses.append("messages.sender IN (?, ?)")
            params.extend([sender_phone_number, canonical_jid(sender_phone_number)])

        if chat_jid:
            where_clauses.append("messages.chat_jid = ?")
            params.append(resolve_chat_jid(cursor, chat_jid))

        if query:
            where_clauses.append("LOWER(messages.content) LIKE LOWER(?) ESCAPE '\\'")
            params.append(f"%{escape_like(query)}%")

        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))

        # Add pagination
        offset = page * limit
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])

        cursor.execute(" ".join(query_parts), tuple(params))
        messages = cursor.fetchall()

        result = []
        for msg in messages:
            message = Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            )
            result.append(message)

        if include_context and result:
            # Add context for each message
            messages_with_context = []
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after, msg.chat_jid)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)

            return format_messages_list(messages_with_context, show_chat_info=True)

        # Format and display messages without context
        return format_messages_list(result, show_chat_info=True)

    finally:
        conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5,
    chat_jid: Optional[str] = None
) -> MessageContext:
    """Get context around a specific message. Message IDs are only unique per
    chat ((id, chat_jid) is the primary key), so pass chat_jid to disambiguate
    when known."""
    conn = connect()
    try:
        cursor = conn.cursor()

        # Get the target message first
        query = """
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.chat_jid, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
        """
        params = [message_id]
        if chat_jid:
            query += " AND messages.chat_jid = ?"
            params.append(resolve_chat_jid(cursor, chat_jid))
        cursor.execute(query, params)
        msg_data = cursor.fetchone()

        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")

        target_message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[8]
        )

        # Get messages before
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ?
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """, (msg_data[7], msg_data[0], before))

        before_messages = []
        for msg in cursor.fetchall():
            before_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))

        # Get messages after
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ?
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """, (msg_data[7], msg_data[0], after))

        after_messages = []
        for msg in cursor.fetchall():
            after_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))

        return MessageContext(
            message=target_message,
            before=before_messages,
            after=after_messages
        )

    finally:
        conn.close()
