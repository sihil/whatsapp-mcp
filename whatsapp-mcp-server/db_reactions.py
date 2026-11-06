"""Reactions on messages: fetched in bulk and attached to already-built
Message objects, so a listing costs one query however many messages it has."""
import sqlite3
from typing import Dict, Iterable, List, Optional, Tuple

from db import placeholders, resolve_sender_names
from models import Message

# Stays well under SQLite's bound-variable limit on older builds.
_CHUNK = 500


def _reactions_available(cursor: sqlite3.Cursor) -> bool:
    """Whether the bridge that wrote this store maintains a reactions table.
    Older stores read as having no reactions."""
    cursor.execute("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'reactions'")
    return cursor.fetchone() is not None


def own_jid(cursor: sqlite3.Cursor) -> Optional[str]:
    """The account's own JID: the bridge stores own PN JID as the sender of
    every is_from_me row, and own reactions use the same spelling. The store
    records the own JID nowhere else, so with no own message yet (a store
    that has only ever received) an own reaction reads as anyone else's."""
    cursor.execute("SELECT sender FROM messages WHERE is_from_me = 1 LIMIT 1")
    row = cursor.fetchone()
    return row[0] if row else None


def attach_reactions(
    cursor: sqlite3.Cursor,
    messages: Iterable[Message],
    name_cache: Optional[Dict[str, Optional[str]]] = None,
) -> None:
    """Fill `Message.reactions` for every message in one pass (chunked only
    past _CHUNK ids). Call once per request with all its messages together,
    context windows included. A message that appears twice (as a match and as
    another's context) shares one list. A store without a reactions table
    leaves every list empty."""
    if not _reactions_available(cursor):
        return
    by_key: Dict[Tuple[str, str], List[Message]] = {}
    for message in messages:
        by_key.setdefault((message.id, message.chat_jid), []).append(message)
    ids = list({key[0] for key in by_key})
    if not ids:
        return

    found: Dict[Tuple[str, str], List[Tuple[str, str]]] = {}
    for start in range(0, len(ids), _CHUNK):
        chunk = ids[start:start + _CHUNK]
        cursor.execute(
            "SELECT message_id, chat_jid, sender, emoji FROM reactions "
            f"WHERE message_id IN ({placeholders(chunk)}) ORDER BY timestamp, sender",
            tuple(chunk),
        )
        for message_id, chat_jid, sender, emoji in cursor.fetchall():
            if (message_id, chat_jid) in by_key and emoji:
                found.setdefault((message_id, chat_jid), []).append((sender, emoji))
    if not found:
        return

    own = next(
        (m.sender for group in by_key.values() for m in group if m.is_from_me), None
    ) or own_jid(cursor)
    senders = [s for pairs in found.values() for s, _ in pairs if s != own]
    names = resolve_sender_names(cursor, senders, name_cache)
    for key, pairs in found.items():
        reactions = [
            {
                "emoji": emoji,
                "sender": sender,
                "sender_name": "Me" if sender == own else names.get(sender, sender),
                "is_from_me": sender == own,
            }
            for sender, emoji in pairs
        ]
        for message in by_key[key]:
            message.reactions = reactions
