"""Read-only tool wrappers. All of these query the bridge's SQLite store
directly (see whatsapp.py) and never touch the network."""
from typing import Annotated, Any, Dict, List, Literal, Optional

from mcp.types import ToolAnnotations
from pydantic import Field

from server import tool
from whatsapp import (
    get_chat as whatsapp_get_chat,
    get_community as whatsapp_get_community,
    get_contact_chats as whatsapp_get_contact_chats,
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
    get_last_interaction as whatsapp_get_last_interaction,
    get_message_context as whatsapp_get_message_context,
    list_chats as whatsapp_list_chats,
    list_communities as whatsapp_list_communities,
    list_group_events as whatsapp_list_group_events,
    list_messages as whatsapp_list_messages,
    list_reactions as whatsapp_list_reactions,
    search_contacts as whatsapp_search_contacts,
)

_READ_ONLY = ToolAnnotations(readOnlyHint=True, openWorldHint=False)


@tool(title="Search Contacts", annotations=_READ_ONLY)
def search_contacts(
    query: Annotated[
        str, Field(description="Search term to match against contact names or phone numbers")
    ],
    limit: Annotated[
        int, Field(description="Maximum number of contacts to return", ge=1, le=200)
    ] = 50,
) -> List[Dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number. Returns a list of
    contacts, each with phone_number, name and jid."""
    return whatsapp_search_contacts(query, limit)


@tool(title="List Messages", annotations=_READ_ONLY)
def list_messages(
    after: Annotated[
        Optional[str], Field(description="Only return messages after this ISO-8601 date")
    ] = None,
    before: Annotated[
        Optional[str], Field(description="Only return messages before this ISO-8601 date")
    ] = None,
    sender_phone_number: Annotated[
        Optional[str], Field(description="Filter messages by sender phone number")
    ] = None,
    chat_jid: Annotated[
        Optional[str], Field(description="Filter messages by chat JID")
    ] = None,
    query: Annotated[
        Optional[str], Field(description="Filter messages by content substring")
    ] = None,
    limit: Annotated[
        int, Field(description="Maximum number of messages to return", ge=1, le=100)
    ] = 20,
    page: Annotated[int, Field(description="Page number for pagination", ge=0)] = 0,
    include_context: Annotated[
        bool, Field(description="Whether to include messages before and after each match")
    ] = True,
    context_before: Annotated[
        int, Field(description="Number of messages to include before each match", ge=0, le=25)
    ] = 1,
    context_after: Annotated[
        int, Field(description="Number of messages to include after each match", ge=0, le=25)
    ] = 1,
) -> List[Dict[str, Any]]:
    """Get WhatsApp messages matching specified criteria with optional context,
    newest first. With include_context each item is {"message": {...},
    "context_before": [...], "context_after": [...]}; without it each item is a
    message dict (id, chat_jid, chat_name, sender, sender_name, timestamp,
    content, is_from_me, media_type, edited, deleted, reactions). A deleted
    message has deleted=true and empty content; reactions is a list of
    {emoji, sender, sender_name, is_from_me}."""
    return whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after,
    )


@tool(title="List Chats", annotations=_READ_ONLY)
def list_chats(
    query: Annotated[
        Optional[str], Field(description="Filter chats by name or JID")
    ] = None,
    limit: Annotated[
        int, Field(description="Maximum number of chats to return", ge=1, le=100)
    ] = 20,
    page: Annotated[int, Field(description="Page number for pagination", ge=0)] = 0,
    include_last_message: Annotated[
        bool, Field(description="Whether to include the last message in each chat")
    ] = True,
    sort_by: Annotated[
        Literal["last_active", "name"], Field(description="Field to sort results by")
    ] = "last_active",
) -> List[Dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria. Returns a list of chat
    dicts (jid, name, is_group, last_message_time, last_message, last_sender,
    last_is_from_me)."""
    return whatsapp_list_chats(
        query=query,
        limit=limit,
        page=page,
        include_last_message=include_last_message,
        sort_by=sort_by,
    )


@tool(title="Get Chat", annotations=_READ_ONLY)
def get_chat(
    chat_jid: Annotated[str, Field(description="The JID of the chat to retrieve")],
    include_last_message: Annotated[
        bool, Field(description="Whether to include the last message")
    ] = True,
) -> Dict[str, Any]:
    """Get WhatsApp chat metadata by JID. Errors when no such chat exists."""
    return whatsapp_get_chat(chat_jid, include_last_message)


@tool(title="Get Direct Chat by Contact", annotations=_READ_ONLY)
def get_direct_chat_by_contact(
    sender_phone_number: Annotated[str, Field(description="The phone number to search for")],
) -> Dict[str, Any]:
    """Get WhatsApp direct (non-group) chat metadata by contact phone number.
    Errors when no chat matches or when several contacts match ambiguously."""
    return whatsapp_get_direct_chat_by_contact(sender_phone_number)


@tool(title="Get Contact's Chats", annotations=_READ_ONLY)
def get_contact_chats(
    jid: Annotated[str, Field(description="The contact's JID to search for")],
    limit: Annotated[
        int, Field(description="Maximum number of chats to return", ge=1, le=100)
    ] = 20,
    page: Annotated[int, Field(description="Page number for pagination", ge=0)] = 0,
) -> List[Dict[str, Any]]:
    """Get all WhatsApp chats involving the contact."""
    return whatsapp_get_contact_chats(jid, limit, page)


@tool(title="Get Last Interaction", annotations=_READ_ONLY)
def get_last_interaction(
    jid: Annotated[str, Field(description="The JID of the contact to search for")],
) -> str:
    """Get most recent WhatsApp message involving the contact, as a formatted
    line; a deleted message reads "[deleted]", an edited one ends "(edited)".
    Errors when there is no message involving the contact."""
    return whatsapp_get_last_interaction(jid)


@tool(title="Get Message Context", annotations=_READ_ONLY)
def get_message_context(
    message_id: Annotated[str, Field(description="The ID of the message to get context for")],
    before: Annotated[
        int,
        Field(
            description="Number of messages to include before the target message", ge=0, le=25
        ),
    ] = 5,
    after: Annotated[
        int,
        Field(
            description="Number of messages to include after the target message", ge=0, le=25
        ),
    ] = 5,
    chat_jid: Annotated[
        Optional[str],
        Field(
            description="JID of the chat the message belongs to. Message IDs are only "
            "unique per chat, so pass this to disambiguate when known."
        ),
    ] = None,
) -> Dict[str, Any]:
    """Get context around a specific WhatsApp message. Returns {"message":
    {...}, "before": [...], "after": [...]}, the context lists in
    chronological order; each message dict carries edited, deleted (deleted
    messages have empty content) and reactions. Errors when the message is not
    found."""
    return whatsapp_get_message_context(message_id, before, after, chat_jid)


@tool(title="List Reactions", annotations=_READ_ONLY)
def list_reactions(
    chat_jid: Annotated[
        Optional[str], Field(description="Only reactions in this chat")
    ] = None,
    after: Annotated[
        Optional[str], Field(description="Only reactions made after this ISO-8601 date")
    ] = None,
    before: Annotated[
        Optional[str], Field(description="Only reactions made before this ISO-8601 date")
    ] = None,
    limit: Annotated[
        int, Field(description="Maximum number of reactions to return", ge=1, le=100)
    ] = 20,
    page: Annotated[int, Field(description="Page number for pagination", ge=0)] = 0,
) -> List[Dict[str, Any]]:
    """Get WhatsApp emoji reactions newest first, by when the reaction was
    made, so a new reaction on an old message shows up. Each item is {emoji,
    sender, sender_name, is_from_me, timestamp, chat_jid, chat_name, message},
    where message is the reacted-to message dict ({"id"} only when the bridge
    never stored it). Removed reactions are not listed. Only reactions received
    since the bridge began recording them are known."""
    return whatsapp_list_reactions(chat_jid, after, before, limit, page)


@tool(title="List Group Membership Changes", annotations=_READ_ONLY)
def list_group_events(
    chat_jid: Annotated[
        Optional[str], Field(description="Only changes in this group (JID ending @g.us)")
    ] = None,
    after: Annotated[
        Optional[str], Field(description="Only changes after this ISO-8601 date")
    ] = None,
    before: Annotated[
        Optional[str], Field(description="Only changes before this ISO-8601 date")
    ] = None,
    limit: Annotated[
        int, Field(description="Maximum number of changes to return", ge=1, le=200)
    ] = 50,
    page: Annotated[int, Field(description="Page number for pagination", ge=0)] = 0,
) -> List[Dict[str, Any]]:
    """Get WhatsApp group membership changes newest first: people joining,
    leaving, and being promoted to or demoted from admin. Each item is
    {chat_jid, chat_name, kind (join, leave, promote, demote), participant,
    participant_name, actor, actor_name, reason, timestamp}. actor is who made
    the change, null when unknown; reason is added, invite, request or
    community for a join and left or removed for a leave, null when unknown.
    Only changes since the bridge began recording them are known; use
    list_group_members for the current roster."""
    return whatsapp_list_group_events(chat_jid, after, before, limit, page)


@tool(title="List Communities", annotations=_READ_ONLY)
def list_communities() -> List[Dict[str, Any]]:
    """List the WhatsApp communities this server can see: each readable
    community, and each community with at least one readable group. Each item
    is {jid, name, readable, subgroups, hidden_subgroups}; subgroups lists the
    readable groups ({jid, name, is_announcement, is_member,
    last_message_time}; is_member says whether the account is in the group,
    null when the bridge has not recorded it) and
    hidden_subgroups counts the community's groups outside this server's
    allowlist. Covers communities the account is in, as of the bridge's last
    sync."""
    return whatsapp_list_communities()


@tool(title="Get Community", annotations=ToolAnnotations(readOnlyHint=True, openWorldHint=True))
def get_community(
    community_jid: Annotated[str, Field(description="The community's JID (ending @g.us)")],
    include_members: Annotated[
        bool,
        Field(description="Also list everyone across the community's groups, fetched live "
              "from WhatsApp. Refused unless every one of its groups is readable."),
    ] = False,
) -> Dict[str, Any]:
    """Get one WhatsApp community: {jid, name, readable, subgroups,
    hidden_subgroups}, plus members and member_count with include_members.
    Each member is {jid, phone_number, display_name}. Errors when the
    community is not readable or is not a community."""
    return whatsapp_get_community(community_jid, include_members)
