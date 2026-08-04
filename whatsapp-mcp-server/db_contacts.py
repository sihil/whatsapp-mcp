"""Contact-oriented queries: finding people, and the chats they appear in."""
from typing import Any, Dict, List

from db import (
    MESSAGE_COLUMNS,
    alias_candidates,
    canonical_jid,
    connect,
    contact_name_expr,
    contacts_available,
    escape_like,
    named,
    resolve_chat_jid,
    chat_from_row,
    message_from_row,
    placeholders,
    resolve_sender_names,
)
from db_chats import _chat_query, format_message
from models import Contact


def search_contacts(query: str, limit: int = 50) -> List[Dict[str, Any]]:
    """Search contacts by name or phone number."""
    conn = connect()
    try:
        cursor = conn.cursor()
        search_pattern = '%' + escape_like(query) + '%'

        # Search everyone the bridge knows of, not only people who have a chat
        # of their own. A group participant who has never sent the user a
        # direct message has no chats row at all, so searching chats alone
        # could not find them by name.
        with_contacts = contacts_available(cursor)
        sources = ["SELECT jid FROM chats WHERE jid NOT LIKE '%@g.us'"]
        name_expr = named("ch.name")
        contact_join = ""
        if with_contacts:
            sources.append("SELECT jid FROM contacts WHERE jid NOT LIKE '%@g.us'")
            name_expr = f"COALESCE({name_expr}, {contact_name_expr('ct.')})"
            contact_join = "LEFT JOIN contacts ct ON ct.jid = cand.jid"

        # A nameless match sorts before every real name, so ordering by name
        # alone lets bare-JID hits fill the whole limit and drop the named
        # contacts the caller was actually looking for. Named ones first.
        cursor.execute(f"""
            SELECT jid, name FROM (
                SELECT cand.jid AS jid, {name_expr} AS name
                FROM ({" UNION ".join(sources)}) cand
                LEFT JOIN chats ch ON ch.jid = cand.jid
                {contact_join}
            )
            WHERE LOWER(name) LIKE LOWER(?) ESCAPE '\\'
               OR LOWER(jid) LIKE LOWER(?) ESCAPE '\\'
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
                cursor,
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
                cursor,
                True,
                f"WHERE c.jid IN ({placeholders(candidates)}) AND c.jid NOT LIKE '%@g.us'",
            ),
            tuple(candidates),
        )
        rows = cursor.fetchall()

        if not rows:
            cursor.execute(
                _chat_query(
                    cursor,
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
                    cursor.execute(_chat_query(cursor, True, "WHERE c.jid = ?"), (merged_jid,))
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
