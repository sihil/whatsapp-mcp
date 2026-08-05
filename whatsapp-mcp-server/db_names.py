"""SQL fragments that turn stored columns into display names.

Every read path builds its name expressions here rather than selecting a raw
`name` column, so one rule about what counts as a name applies to all of them.
Get this wrong in one query and the same chat is named by one tool and nameless
in another.
"""
import sqlite3


def named(column: str) -> str:
    """SQL yielding `column` only when it holds an actual name.

    A value of nothing but digits is the phone number some lookup fell back to,
    not a name, and it is worse than NULL: it looks like a successful lookup and
    stops the caller's fallback chain. The bridge no longer writes them and
    clears the old ones at startup (see whatsapp-bridge/contacts.go), but a
    store whose bridge has not been restarted still has them, so the read path
    rejects them too.

    Trimmed first, so a blank is not mistaken for a name: ' ' contains a
    non-digit and would otherwise pass, shadowing a real name from a weaker
    source exactly as a phone number does.
    """
    return f"CASE WHEN TRIM({column}) GLOB '*[^0-9]*' THEN TRIM({column}) END"


def contact_name_expr(prefix: str = "") -> str:
    """SQL for a contact's display name, best source first. The bridge fills
    these from the address book, from the push name riding on every incoming
    message, and from group participant lookups. `prefix` qualifies the columns
    when the contacts table is joined under an alias."""
    return (
        f"COALESCE({named(prefix + 'full_name')}, "
        f"{named(prefix + 'push_name')}, {named(prefix + 'business_name')})"
    )


def contacts_available(cursor: sqlite3.Cursor) -> bool:
    """Whether the bridge that wrote this store maintains a contacts table.
    Stores written before it existed keep working, without sender names, until
    the bridge is restarted. Probe once per request and pass the result to the
    builders below — they all need it."""
    cursor.execute("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'contacts'")
    return cursor.fetchone() is not None


def contacts_join(with_contacts: bool, chat: str = "c", contact: str = "ct") -> str:
    """The join `chat_name_expr` reads from, or nothing when the store has no
    contacts table."""
    if not with_contacts:
        return ""
    return f"LEFT JOIN contacts {contact} ON {contact}.jid = {chat}.jid"


def chat_name_expr(with_contacts: bool, chat: str = "c", contact: str = "ct") -> str:
    """SQL for a chat's display name: the name stored on the chat, else the
    contact behind it.

    A direct chat's own JID is the contact's JID, so the fallback names
    conversations the bridge could not name at the time it first saw them —
    including every chat whose stored name was a bare phone number until the
    bridge cleared it. Select, filter and order by this rather than a raw
    `name` column; pair it with `contacts_join`.

    Multi-party chats are exempt from the digits rule: a group or channel name
    is set by a person and has no contact behind the JID to fall back to, so
    "2024" is a real name there. Matches isMultiPartyChat in the bridge.
    """
    multi_party = " OR ".join(
        f"{chat}.jid LIKE '%@{server}'" for server in ("g.us", "broadcast", "newsletter")
    )
    name = (
        f"CASE WHEN {multi_party} THEN NULLIF(TRIM({chat}.name), '') "
        f"ELSE {named(chat + '.name')} END"
    )
    if not with_contacts:
        return name
    return f"COALESCE({name}, {contact_name_expr(contact + '.')})"


def message_source(with_contacts: bool) -> str:
    """SELECT ... FROM for message rows, in the column order `message_from_row`
    expects. The chat name goes through `chat_name_expr`, so a message reports
    the same chat name `list_chats` would."""
    return f"""
        SELECT messages.id, messages.chat_jid,
               {chat_name_expr(with_contacts, "chats")},
               messages.sender, messages.content, messages.timestamp,
               messages.is_from_me, messages.media_type
        FROM messages
        JOIN chats ON messages.chat_jid = chats.jid
        {contacts_join(with_contacts, "chats")}
    """
