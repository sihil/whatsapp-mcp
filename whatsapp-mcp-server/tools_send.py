"""Send / live-action tool wrappers. These call the bridge's REST API (see
whatsapp.py, api_client.py) rather than reading SQLite directly."""
from typing import Any, Dict, Optional

from server import mcp
from whatsapp import (
    download_media as whatsapp_download_media,
    get_group_members as whatsapp_get_group_members,
    send_audio_message as whatsapp_send_audio_message,
    send_file as whatsapp_send_file,
    send_message as whatsapp_send_message,
)


@mcp.tool()
def send_message(
    recipient: str,
    message: str,
    reply_to: Optional[str] = None
) -> Dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.
    Raises on failure (invalid recipient, unreachable bridge, or a rejected send).

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send
        reply_to: Optional message ID (as shown by list_messages) to quote. When provided, the
                 message is sent as a quoted reply to that message in the recipient chat.

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_send_message(recipient, message, reply_to)
    if not success:
        raise RuntimeError(status_message)
    return {"success": True, "message": status_message}


@mcp.tool()
def send_file(recipient: str, media_path: str, reply_to: Optional[str] = None) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via WhatsApp to the specified recipient. For group messages use the JID.
    Raises on failure (invalid recipient/path, unreachable bridge, or a rejected send).

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the media file to send (image, video, document)
        reply_to: Optional message ID (as shown by list_messages) to quote. When provided, the
                 media is sent as a quoted reply to that message in the recipient chat.

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_send_file(recipient, media_path, reply_to)
    if not success:
        raise RuntimeError(status_message)
    return {"success": True, "message": status_message}


@mcp.tool()
def send_audio_message(recipient: str, media_path: str, reply_to: Optional[str] = None) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.
    Raises on failure (invalid recipient/path, conversion failure, unreachable bridge, or a rejected send).

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)
        reply_to: Optional message ID (as shown by list_messages) to quote. When provided, the
                 voice message is sent as a quoted reply to that message in the recipient chat.

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_send_audio_message(recipient, media_path, reply_to)
    if not success:
        raise RuntimeError(status_message)
    return {"success": True, "message": status_message}


@mcp.tool()
def download_media(message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.
    Raises on failure (message/media not found, unreachable bridge, or a
    rejected download), with the bridge's actual error text.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        A dictionary containing success status, a status message, and the file path
    """
    file_path, status_message = whatsapp_download_media(message_id, chat_jid)
    if not file_path:
        raise RuntimeError(status_message)
    return {
        "success": True,
        "message": status_message,
        "file_path": file_path,
    }


@mcp.tool()
def list_group_members(group_jid: str) -> Dict[str, Any]:
    """Get members of a WhatsApp group. Raises on failure (group not found,
    unreachable bridge).

    Args:
        group_jid: The JID of the group (must end with @g.us)

    Returns:
        A dictionary containing group name, member count, and list of members
        with their JID, phone number, display name, and admin status
    """
    result = whatsapp_get_group_members(group_jid)
    if not result.get("success", False):
        # Passing the soft failure through would report an unreachable bridge
        # as a group with zero members.
        raise RuntimeError(result.get("message", "Unknown error"))
    return result
