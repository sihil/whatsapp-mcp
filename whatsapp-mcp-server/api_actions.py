"""Reactions, edits and deletes of existing messages, sent through the bridge.

Shares api_client's rate limiter and transport with the send tools: these are
outbound WhatsApp traffic too. Validation failures raise ValueError; bridge
communication failures come back as a (False, message) tuple.
"""
from typing import Tuple

import access_control
from api_client import post_limited

# Small JSON round trips, unlike a media upload.
_ACTION_TIMEOUT = (5, 30)


def _require(**fields: str) -> None:
    for name, value in fields.items():
        if not value:
            raise ValueError(f"{name.replace('_', ' ').capitalize()} must be provided")
    access_control.check_writable(fields["chat_jid"])


def send_reaction(chat_jid: str, message_id: str, emoji: str) -> Tuple[bool, str]:
    """React to a message; an empty `emoji` removes the reaction."""
    _require(chat_jid=chat_jid, message_id=message_id)
    return post_limited(
        "/react",
        {"chat_jid": chat_jid, "message_id": message_id, "emoji": emoji or ""},
        _ACTION_TIMEOUT,
    )


def edit_message(chat_jid: str, message_id: str, message: str) -> Tuple[bool, str]:
    """Replace the text of one of our own messages."""
    _require(chat_jid=chat_jid, message_id=message_id)
    if not message or not message.strip():
        raise ValueError("Message must be provided")
    return post_limited(
        "/edit",
        {"chat_jid": chat_jid, "message_id": message_id, "message": message},
        _ACTION_TIMEOUT,
    )


def delete_message(chat_jid: str, message_id: str) -> Tuple[bool, str]:
    """Delete one of our own messages for everyone."""
    _require(chat_jid=chat_jid, message_id=message_id)
    return post_limited(
        "/delete", {"chat_jid": chat_jid, "message_id": message_id}, _ACTION_TIMEOUT
    )
