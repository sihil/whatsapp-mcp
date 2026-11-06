"""Message queries against the bridge's message store."""
import logging
import sqlite3
from typing import Any, Dict, List, Optional, Sequence, Tuple

import access_control
from db import (
    DATE_EXPR,
    alias_candidates,
    canonical_jid,
    connect,
    escape_like,
    resolve_chat_jid,
    messages_from_rows,
    parse_date_param,
    placeholders,
)
from db_names import message_source
from db_reactions import attach_reactions
from models import Message, MessageContext

logger = logging.getLogger(__name__)


def _context_around(
    cursor: sqlite3.Cursor,
    source: str,
    chat_jid: str,
    message_id: str,
    timestamp: str,
    before: int,
    after: int,
    name_cache: Optional[Dict[str, Optional[str]]] = None,
) -> Tuple[List[Message], List[Message]]:
    """Messages immediately before and after `timestamp` in one chat, both
    lists in chronological order. `source` is the caller's `message_source`,
    passed in so the sqlite_master probe behind it happens once per request
    rather than once per match. `name_cache` is shared across the calls made
    for one listing so sender names resolve once, not once per match.

    Ordering is by (timestamp, id) rather than timestamp alone: WhatsApp
    timestamps are second-granularity, so a rapid-fire exchange routinely puts
    several messages in the same second. A strict comparison on timestamp
    alone would drop every one of those neighbours from the window, while an
    inclusive one would place each of them in *both* windows; the id tiebreak
    gives a total order, so each neighbour lands on exactly one side.
    """
    before_messages: List[Message] = []
    if before > 0:
        cursor.execute(f"""
            {source}
            WHERE messages.chat_jid = ?
              AND (messages.timestamp, messages.id) < (?, ?)
            ORDER BY messages.timestamp DESC, messages.id DESC
            LIMIT ?
        """, (chat_jid, timestamp, message_id, before))
        before_messages = messages_from_rows(cursor, cursor.fetchall(), name_cache)
        before_messages.reverse()

    after_messages: List[Message] = []
    if after > 0:
        cursor.execute(f"""
            {source}
            WHERE messages.chat_jid = ?
              AND (messages.timestamp, messages.id) > (?, ?)
            ORDER BY messages.timestamp ASC, messages.id ASC
            LIMIT ?
        """, (chat_jid, timestamp, message_id, after))
        after_messages = messages_from_rows(cursor, cursor.fetchall(), name_cache)

    return before_messages, after_messages


def _build_message_filters(
    cursor: sqlite3.Cursor,
    after: Optional[str],
    before: Optional[str],
    sender_phone_number: Optional[str],
    chat_jid: Optional[str],
    query: Optional[str],
) -> Tuple[List[str], List[Any]]:
    allowed, allowed_params = access_control.read_filter(cursor, "messages.chat_jid")
    where_clauses: List[str] = [allowed]
    params: List[Any] = list(allowed_params)

    if after:
        where_clauses.append(f"{DATE_EXPR} > datetime(?)")
        params.append(parse_date_param(after, "after"))

    if before:
        where_clauses.append(f"{DATE_EXPR} < datetime(?)")
        params.append(parse_date_param(before, "before"))

    if sender_phone_number:
        # The sender column stores full canonical JIDs; also match the raw
        # value and any alias spellings the bridge has since merged.
        candidates = alias_candidates(cursor, canonical_jid(sender_phone_number))
        if sender_phone_number not in candidates:
            candidates.append(sender_phone_number)
        where_clauses.append(f"messages.sender IN ({placeholders(candidates)})")
        params.extend(candidates)

    if chat_jid:
        resolved_jid = resolve_chat_jid(cursor, canonical_jid(chat_jid))
        access_control.check_readable(resolved_jid, cursor)
        where_clauses.append("messages.chat_jid = ?")
        params.append(resolved_jid)

    if query:
        where_clauses.append("LOWER(messages.content) LIKE LOWER(?) ESCAPE '\\'")
        params.append(f"%{escape_like(query)}%")

    return where_clauses, params


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
) -> List[Dict[str, Any]]:
    """Get messages matching the specified criteria, newest first.

    Without context each item is a message dict; with context each item is
    {"message": {...}, "context_before": [...], "context_after": [...]}.
    """
    conn = connect()
    try:
        cursor = conn.cursor()
        source = message_source(cursor)
        where_clauses, params = _build_message_filters(
            cursor, after, before, sender_phone_number, chat_jid, query
        )

        query_parts = [source]
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, page * limit])

        cursor.execute(" ".join(query_parts), tuple(params))
        rows: Sequence[Sequence[Any]] = cursor.fetchall()
        name_cache: Dict[str, Optional[str]] = {}
        messages = messages_from_rows(cursor, rows, name_cache)

        if not include_context:
            attach_reactions(cursor, messages, name_cache)
            return [message.to_dict() for message in messages]

        windows: List[Tuple[List[Message], List[Message]]] = []
        for row, message in zip(rows, messages):
            try:
                context_before_msgs, context_after_msgs = _context_around(
                    cursor, source, message.chat_jid, message.id, row[5],
                    context_before, context_after, name_cache
                )
            except sqlite3.Error as e:
                # One failed context lookup must not lose the whole listing.
                logger.warning(f"Could not load context for message {message.id}: {e}")
                context_before_msgs, context_after_msgs = [], []
            windows.append((context_before_msgs, context_after_msgs))

        # One bulk fetch for the matches and every context window.
        context = [msg for window in windows for side in window for msg in side]
        attach_reactions(cursor, [*messages, *context], name_cache)
        return [
            {
                "message": message.to_dict(),
                "context_before": [msg.to_dict() for msg in window_before],
                "context_after": [msg.to_dict() for msg in window_after],
            }
            for message, (window_before, window_after) in zip(messages, windows)
        ]
    finally:
        conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5,
    chat_jid: Optional[str] = None
) -> Dict[str, Any]:
    """Get context around a specific message. Message IDs are only unique per
    chat ((id, chat_jid) is the primary key), so pass chat_jid to disambiguate
    when known."""
    conn = connect()
    try:
        cursor = conn.cursor()
        source = message_source(cursor)
        allowed, allowed_params = access_control.read_filter(cursor, "messages.chat_jid")
        query = f"{source} WHERE messages.id = ? AND {allowed}"
        params: List[Any] = [message_id, *allowed_params]
        if chat_jid:
            resolved_jid = resolve_chat_jid(cursor, canonical_jid(chat_jid))
            access_control.check_readable(resolved_jid, cursor)
            query += " AND messages.chat_jid = ?"
            params.append(resolved_jid)

        cursor.execute(query, tuple(params))
        row = cursor.fetchone()
        if not row:
            raise ValueError(f"Message with ID {message_id} not found")

        name_cache: Dict[str, Optional[str]] = {}
        target = messages_from_rows(cursor, [row], name_cache)[0]
        before_messages, after_messages = _context_around(
            cursor, source, target.chat_jid, target.id, row[5], before, after, name_cache
        )
        attach_reactions(cursor, [target, *before_messages, *after_messages], name_cache)
        return MessageContext(
            message=target, before=before_messages, after=after_messages
        ).to_dict()
    finally:
        conn.close()
