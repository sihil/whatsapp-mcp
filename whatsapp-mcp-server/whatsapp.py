"""Façade over the WhatsApp MCP implementation modules.

Reads (`db*`) query the bridge's SQLite store directly; writes and live
actions (`api_client`) go through the bridge's REST API.
"""
from api_client import (
    download_media,
    get_group_members,
    send_audio_message,
    send_file,
    send_message,
)
from config import api_base_url, messages_db_path
from db import get_sender_name
from db_chats import (
    format_message,
    get_chat,
    get_contact_chats,
    get_direct_chat_by_contact,
    get_last_interaction,
    list_chats,
    search_contacts,
)
from db_messages import get_message_context, list_messages
from models import Chat, Contact, Message, MessageContext

__all__ = [
    "Chat",
    "Contact",
    "Message",
    "MessageContext",
    "api_base_url",
    "download_media",
    "format_message",
    "get_chat",
    "get_contact_chats",
    "get_direct_chat_by_contact",
    "get_group_members",
    "get_last_interaction",
    "get_message_context",
    "get_sender_name",
    "list_chats",
    "list_messages",
    "messages_db_path",
    "search_contacts",
    "send_audio_message",
    "send_file",
    "send_message",
]
