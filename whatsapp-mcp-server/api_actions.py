"""Reactions, edits and deletes of existing messages, sent through the bridge.

Shares api_client's rate limiter and transport with the send tools: these are
outbound WhatsApp traffic too. Validation failures raise ValueError; bridge
communication failures come back as a (False, message) tuple.
"""
from typing import Dict, Tuple

from api_client import _post, _release_send_slot, _reserve_send_slot

# Small JSON round trips, unlike a media upload.
_ACTION_TIMEOUT = (5, 30)


def _post_action(endpoint: str, payload: Dict[str, str]) -> Tuple[bool, str]:
    """Claim a rate-limit slot, POST to the bridge, and parse the result. The
    slot is handed back when the request never reached the bridge."""
    reserved = _reserve_send_slot()
    body, error = _post(endpoint, payload, timeout=_ACTION_TIMEOUT)
    if body is None:
        _release_send_slot(reserved)
        return False, error
    return body.get("success", False), body.get("message", "Unknown response")


def _require(**fields: str) -> None:
    for name, value in fields.items():
        if not value:
            raise ValueError(f"{name.replace('_', ' ').capitalize()} must be provided")


def send_reaction(chat_jid: str, message_id: str, emoji: str) -> Tuple[bool, str]:
    """React to a message; an empty `emoji` removes the reaction."""
    _require(chat_jid=chat_jid, message_id=message_id)
    return _post_action(
        "/react", {"chat_jid": chat_jid, "message_id": message_id, "emoji": emoji or ""}
    )


def edit_message(chat_jid: str, message_id: str, message: str) -> Tuple[bool, str]:
    """Replace the text of one of our own messages."""
    _require(chat_jid=chat_jid, message_id=message_id)
    if not message or not message.strip():
        raise ValueError("Message must be provided")
    return _post_action(
        "/edit", {"chat_jid": chat_jid, "message_id": message_id, "message": message}
    )


def delete_message(chat_jid: str, message_id: str) -> Tuple[bool, str]:
    """Delete one of our own messages for everyone."""
    _require(chat_jid=chat_jid, message_id=message_id)
    return _post_action("/delete", {"chat_jid": chat_jid, "message_id": message_id})
