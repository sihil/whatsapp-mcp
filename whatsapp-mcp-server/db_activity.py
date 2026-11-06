"""Activity that is not a message of its own: reactions, listed by when they
were made rather than by the message they are on, and group membership
changes."""
import sqlite3
from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple

import access_control
from db import (
    canonical_jid,
    connect,
    messages_from_rows,
    parse_date_param,
    resolve_chat_jid,
    resolve_sender_names,
)
from db_names import chat_name_expr, contacts_available, contacts_join, message_source
from db_reactions import own_jid


def _table_available(cursor: sqlite3.Cursor, table: str) -> bool:
    """Stores written before the bridge kept this table read as empty."""
    cursor.execute("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", (table,))
    return cursor.fetchone() is not None


def _filters(
    cursor: sqlite3.Cursor,
    alias: str,
    chat_jid: Optional[str],
    after: Optional[str],
    before: Optional[str],
) -> Tuple[str, List[Any]]:
    allowed, params = access_control.read_filter(cursor, f"{alias}.chat_jid")
    clauses = [allowed]
    if chat_jid:
        resolved = resolve_chat_jid(cursor, canonical_jid(chat_jid))
        access_control.check_readable(resolved, cursor)
        clauses.append(f"{alias}.chat_jid = ?")
        params.append(resolved)
    if after:
        clauses.append(f"datetime({alias}.timestamp) > datetime(?)")
        params.append(parse_date_param(after, "after"))
    if before:
        clauses.append(f"datetime({alias}.timestamp) < datetime(?)")
        params.append(parse_date_param(before, "before"))
    return " AND ".join(clauses), params


def _iso(stored: str) -> str:
    return datetime.fromisoformat(stored).isoformat()


def list_reactions(
    chat_jid: Optional[str] = None,
    after: Optional[str] = None,
    before: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
) -> List[Dict[str, Any]]:
    """Reactions newest first, each with the message it is on. A reaction
    that was later removed is not listed."""
    conn = connect()
    try:
        cursor = conn.cursor()
        if not _table_available(cursor, "reactions"):
            return []
        where, params = _filters(cursor, "r", chat_jid, after, before)
        with_contacts = contacts_available(cursor)
        cursor.execute(
            f"SELECT r.message_id, r.chat_jid, r.sender, r.emoji, r.timestamp, "
            f"{chat_name_expr(with_contacts)} FROM reactions r "
            f"LEFT JOIN chats c ON c.jid = r.chat_jid {contacts_join(with_contacts)} "
            f"WHERE r.emoji <> '' AND {where} "
            f"ORDER BY datetime(r.timestamp) DESC LIMIT ? OFFSET ?",
            (*params, limit, page * limit),
        )
        rows = cursor.fetchall()

        source = message_source(cursor)
        name_cache: Dict[str, Optional[str]] = {}
        targets = {}
        for message_id, chat, *_ in rows:
            cursor.execute(
                f"{source} WHERE messages.id = ? AND messages.chat_jid = ?", (message_id, chat)
            )
            found = cursor.fetchall()
            if found:
                targets[(message_id, chat)] = messages_from_rows(cursor, found, name_cache)[0]

        own = own_jid(cursor)
        names = resolve_sender_names(cursor, [row[2] for row in rows if row[2] != own], name_cache)
        result = []
        for message_id, chat, sender, emoji, timestamp, chat_name in rows:
            target = targets.get((message_id, chat))
            result.append({
                "emoji": emoji,
                "sender": sender,
                "sender_name": "Me" if sender == own else names.get(sender, sender),
                "is_from_me": sender == own,
                "timestamp": _iso(timestamp),
                "chat_jid": chat,
                "chat_name": chat_name,
                "message": target.to_dict() if target else {"id": message_id},
            })
        return result
    finally:
        conn.close()


def list_group_events(
    chat_jid: Optional[str] = None,
    after: Optional[str] = None,
    before: Optional[str] = None,
    limit: int = 50,
    page: int = 0,
) -> List[Dict[str, Any]]:
    """Group membership changes newest first: joins, leaves, promotions and
    demotions, with who made the change when known."""
    conn = connect()
    try:
        cursor = conn.cursor()
        if not _table_available(cursor, "group_events"):
            return []
        where, params = _filters(cursor, "e", chat_jid, after, before)
        cursor.execute(
            f"SELECT e.chat_jid, {chat_name_expr(False)}, e.kind, e.participant, e.actor, "
            f"e.reason, e.timestamp FROM group_events e "
            f"LEFT JOIN chats c ON c.jid = e.chat_jid "
            f"WHERE {where} ORDER BY datetime(e.timestamp) DESC, e.kind, e.participant "
            f"LIMIT ? OFFSET ?",
            (*params, limit, page * limit),
        )
        rows = cursor.fetchall()

        own = own_jid(cursor)
        people = [jid for row in rows for jid in (row[3], row[4]) if jid and jid != own]
        names = resolve_sender_names(cursor, people)

        def name_of(jid: str) -> Optional[str]:
            if not jid:
                return None
            return "Me" if jid == own else names.get(jid, jid)

        return [
            {
                "chat_jid": chat,
                "chat_name": chat_name,
                "kind": kind,
                "participant": participant,
                "participant_name": name_of(participant),
                "actor": actor or None,
                "actor_name": name_of(actor),
                "reason": reason or None,
                "timestamp": _iso(timestamp),
            }
            for chat, chat_name, kind, participant, actor, reason, timestamp in rows
        ]
    finally:
        conn.close()
