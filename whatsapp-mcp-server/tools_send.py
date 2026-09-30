"""Send / live-action tool wrappers. These call the bridge's REST API (see
whatsapp.py, api_client.py) rather than reading SQLite directly."""
from typing import Annotated, Any, Dict, Optional, Tuple

from mcp.types import ToolAnnotations
from pydantic import Field

from server import mcp
from whatsapp import (
    delete_message as whatsapp_delete_message,
    edit_message as whatsapp_edit_message,
    download_media as whatsapp_download_media,
    get_group_members as whatsapp_get_group_members,
    send_audio_message as whatsapp_send_audio_message,
    send_file as whatsapp_send_file,
    send_message as whatsapp_send_message,
    send_reaction as whatsapp_send_reaction,
)

_RECIPIENT_DESC = (
    'The recipient - either a phone number with country code but no + or other '
    'symbols, or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like '
    '"123456789@g.us")'
)
_CHAT_JID_DESC = "The JID of the chat containing the message"
_MESSAGE_ID_DESC = "The ID of the message (as shown by list_messages)"

_SEND = ToolAnnotations(
    readOnlyHint=False, destructiveHint=False, idempotentHint=False, openWorldHint=True
)
# Download, react and edit: repeating the call leaves the same end state.
_IDEMPOTENT = ToolAnnotations(
    readOnlyHint=False, destructiveHint=False, idempotentHint=True, openWorldHint=True
)
_DELETE = ToolAnnotations(
    readOnlyHint=False, destructiveHint=True, idempotentHint=True, openWorldHint=True
)
_GROUP_MEMBERS = ToolAnnotations(readOnlyHint=True, openWorldHint=True)


def _result(outcome: Tuple[bool, str]) -> Dict[str, Any]:
    """Turn an api (success, message) pair into the tool's return value,
    raising on failure so the client sees an error rather than a soft one."""
    success, status_message = outcome
    if not success:
        raise RuntimeError(status_message)
    return {"success": True, "message": status_message}


@mcp.tool(title="Send Message", annotations=_SEND)
def send_message(
    recipient: Annotated[str, Field(description=_RECIPIENT_DESC)],
    message: Annotated[str, Field(description="The message text to send")],
    reply_to: Annotated[
        Optional[str],
        Field(
            description="Optional message ID (as shown by list_messages) to quote. When "
            "provided, the message is sent as a quoted reply to that message in the "
            "recipient chat."
        ),
    ] = None,
) -> Dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the
    JID. Returns a dict with success status and a status message, which carries
    the new message's ID (the handle for edit_message / delete_message). Raises on
    failure (invalid recipient, unreachable bridge, or a rejected send)."""
    return _result(whatsapp_send_message(recipient, message, reply_to))


@mcp.tool(title="Send File", annotations=_SEND)
def send_file(
    recipient: Annotated[str, Field(description=_RECIPIENT_DESC)],
    media_path: Annotated[
        str, Field(description="The absolute path to the media file to send (image, video, document)")
    ],
    reply_to: Annotated[
        Optional[str],
        Field(
            description="Optional message ID (as shown by list_messages) to quote. When "
            "provided, the media is sent as a quoted reply to that message in the "
            "recipient chat."
        ),
    ] = None,
) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via
    WhatsApp to the specified recipient. For group messages use the JID.
    Returns a dict with success status and a status message. Raises on
    failure (invalid recipient/path, path outside the allowed media
    directories, unreachable bridge, or a rejected send)."""
    return _result(whatsapp_send_file(recipient, media_path, reply_to))


@mcp.tool(title="Send Audio Message", annotations=_SEND)
def send_audio_message(
    recipient: Annotated[str, Field(description=_RECIPIENT_DESC)],
    media_path: Annotated[
        str,
        Field(
            description="The absolute path to the audio file to send (will be converted "
            "to Opus .ogg if it's not a .ogg file)"
        ),
    ],
    reply_to: Annotated[
        Optional[str],
        Field(
            description="Optional message ID (as shown by list_messages) to quote. When "
            "provided, the voice message is sent as a quoted reply to that message in the "
            "recipient chat."
        ),
    ] = None,
) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified
    recipient. For group messages use the JID. If it errors due to ffmpeg not
    being installed, use send_file instead. Returns a dict with success status
    and a status message. Raises on failure (invalid recipient/path, path
    outside the allowed media directories, conversion failure, unreachable
    bridge, or a rejected send)."""
    return _result(whatsapp_send_audio_message(recipient, media_path, reply_to))


@mcp.tool(title="Download Media", annotations=_IDEMPOTENT)
def download_media(
    message_id: Annotated[str, Field(description="The ID of the message containing the media")],
    chat_jid: Annotated[str, Field(description=_CHAT_JID_DESC)],
) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.
    Returns a dict with success status, a status message, and the file path.
    Raises on failure (message/media not found, unreachable bridge, or a
    rejected download), with the bridge's actual error text."""
    file_path, status_message = whatsapp_download_media(message_id, chat_jid)
    if not file_path:
        raise RuntimeError(status_message)
    return {
        "success": True,
        "message": status_message,
        "file_path": file_path,
    }


@mcp.tool(title="List Group Members", annotations=_GROUP_MEMBERS)
def list_group_members(
    group_jid: Annotated[str, Field(description="The JID of the group (must end with @g.us)")],
) -> Dict[str, Any]:
    """Get members of a WhatsApp group. Returns a dict with group name, member
    count, and list of members with their JID, phone number, display name, and
    admin status. Raises on failure (group not found, unreachable bridge)."""
    result = whatsapp_get_group_members(group_jid)
    if not result.get("success", False):
        # Passing the soft failure through would report an unreachable bridge
        # as a group with zero members.
        raise RuntimeError(result.get("message", "Unknown error"))
    return result


@mcp.tool(title="Send Reaction", annotations=_IDEMPOTENT)
def send_reaction(
    chat_jid: Annotated[str, Field(description=_CHAT_JID_DESC)],
    message_id: Annotated[str, Field(description=_MESSAGE_ID_DESC)],
    emoji: Annotated[
        str,
        Field(description="The reaction emoji, e.g. \"👍\". Pass an empty string to remove "
              "your existing reaction."),
    ],
) -> Dict[str, Any]:
    """React to a WhatsApp message with an emoji, replacing any earlier
    reaction of yours on it, or remove your reaction with an empty emoji.
    Returns a dict with success status and a status message. Raises on failure
    (message not found, unreachable bridge, or a rejected send)."""
    return _result(whatsapp_send_reaction(chat_jid, message_id, emoji))


@mcp.tool(title="Edit Message", annotations=_IDEMPOTENT)
def edit_message(
    chat_jid: Annotated[str, Field(description=_CHAT_JID_DESC)],
    message_id: Annotated[str, Field(description=_MESSAGE_ID_DESC)],
    message: Annotated[str, Field(description="The new text for the message")],
) -> Dict[str, Any]:
    """Edit the text of a message you sent. Only your own text messages can be
    edited, only within 15 minutes of sending, and not once deleted. Returns a
    dict with success status and a status message. Raises on failure (message
    not found or not editable, unreachable bridge, or a rejected send)."""
    return _result(whatsapp_edit_message(chat_jid, message_id, message))


@mcp.tool(title="Delete Message", annotations=_DELETE)
def delete_message(
    chat_jid: Annotated[str, Field(description=_CHAT_JID_DESC)],
    message_id: Annotated[str, Field(description=_MESSAGE_ID_DESC)],
) -> Dict[str, Any]:
    """Delete a message you sent, for everyone in the chat ("delete for
    everyone"), only within 2 days 12 hours of sending (WhatsApp's limit). This
    cannot be undone. Afterwards the message still appears in listings as
    deleted, with empty content. Returns a dict with success status and a status
    message. Raises on failure (message not found, not yours or too old,
    unreachable bridge, or a rejected send)."""
    return _result(whatsapp_delete_message(chat_jid, message_id))
