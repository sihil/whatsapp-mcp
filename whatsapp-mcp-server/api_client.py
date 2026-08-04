"""REST calls to the Go bridge (sends, media downloads, live group data)."""
import json
import os.path
from typing import Dict, Optional, Tuple

import requests

import audio
from config import WHATSAPP_API_BASE_URL


def _post_send(payload: Dict[str, str]) -> Tuple[bool, str]:
    """POST a payload to the bridge's /send endpoint and parse the result.

    The bridge returns a {success, message} JSON body on both 200 and
    error responses; only its plain-text validation errors are not JSON.
    """
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/send", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
        return result.get("success", False), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def send_message(recipient: str, message: str, reply_to: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"

    payload = {
        "recipient": recipient,
        "message": message,
    }
    if reply_to:
        payload["reply_to"] = reply_to
    return _post_send(payload)


def send_file(recipient: str, media_path: str, reply_to: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"

    if not media_path:
        return False, "Media path must be provided"

    if not os.path.isfile(media_path):
        return False, f"Media file not found: {media_path}"

    payload = {
        "recipient": recipient,
        "media_path": media_path
    }
    if reply_to:
        payload["reply_to"] = reply_to
    return _post_send(payload)


def send_audio_message(recipient: str, media_path: str, reply_to: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"

    if not media_path:
        return False, "Media path must be provided"

    if not os.path.isfile(media_path):
        return False, f"Media file not found: {media_path}"

    if not media_path.endswith(".ogg"):
        try:
            media_path = audio.convert_to_opus_ogg_temp(media_path)
        except Exception as e:
            return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"

    payload = {
        "recipient": recipient,
        "media_path": media_path
    }
    if reply_to:
        payload["reply_to"] = reply_to
    return _post_send(payload)


def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a message and return the local file path.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        The local file path if download was successful, None otherwise
    """
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {
            "message_id": message_id,
            "chat_jid": chat_jid
        }

        response = requests.post(url, json=payload)

        if response.status_code == 200:
            result = response.json()
            if result.get("success", False):
                path = result.get("path")
                print(f"Media downloaded successfully: {path}")
                return path
            else:
                print(f"Download failed: {result.get('message', 'Unknown error')}")
                return None
        else:
            print(f"Error: HTTP {response.status_code} - {response.text}")
            return None

    except requests.RequestException as e:
        print(f"Request error: {str(e)}")
        return None
    except json.JSONDecodeError:
        print(f"Error parsing response: {response.text}")
        return None
    except Exception as e:
        print(f"Unexpected error: {str(e)}")
        return None


def get_group_members(group_jid: str) -> dict:
    """Get members of a WhatsApp group.

    Args:
        group_jid: The JID of the group (must end with @g.us)

    Returns:
        A dictionary containing success status, group name, member count, and list of members
    """
    try:
        url = f"{WHATSAPP_API_BASE_URL}/group/members"
        payload = {
            "group_jid": group_jid
        }

        response = requests.post(url, json=payload)

        if response.status_code == 200:
            result = response.json()
            return result
        else:
            return {
                "success": False,
                "message": f"Error: HTTP {response.status_code} - {response.text}",
                "members": [],
                "member_count": 0
            }

    except requests.RequestException as e:
        return {
            "success": False,
            "message": f"Request error: {str(e)}",
            "members": [],
            "member_count": 0
        }
    except json.JSONDecodeError:
        return {
            "success": False,
            "message": f"Error parsing response: {response.text}",
            "members": [],
            "member_count": 0
        }
    except Exception as e:
        return {
            "success": False,
            "message": f"Unexpected error: {str(e)}",
            "members": [],
            "member_count": 0
        }
