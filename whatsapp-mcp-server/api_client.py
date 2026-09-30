"""REST calls to the Go bridge (sends, media downloads, live group data).

Send-path validation lives here rather than in the tool wrappers: recipient/
media-path checks, the media directory allowlist, and the sliding-window
rate limiter all apply regardless of caller. Validation failures raise
ValueError; bridge communication failures come back as a (False, message)
tuple for the caller to turn into a RuntimeError.
"""
import json
import logging
import os
import threading
import time
from collections import deque
from pathlib import Path
from typing import Any, Deque, Dict, Optional, Tuple

import requests

import audio
from config import BRIDGE_DOWN_HINT, allowed_media_dirs, api_base_url, send_rate_limit

logger = logging.getLogger(__name__)

# A /send call blocks for the bridge's entire upload to WhatsApp, so this
# bounds a large media transfer over a slow link, not a quick round trip.
_SEND_READ_TIMEOUT = 600

_send_timestamps: Deque[float] = deque()
_send_lock = threading.Lock()


def _reserve_send_slot() -> Optional[float]:
    """Claim a slot in the sliding send window, raising if this call would
    exceed WHATSAPP_MCP_SEND_RATE_LIMIT sends in the trailing 60 seconds.
    Shared by every outbound tool (see `post_limited`); `<= 0` disables the limit.

    Returns the recorded timestamp so a send that never reached the bridge can
    hand the slot back via `_release_send_slot`, or None when disabled."""
    limit = send_rate_limit()
    if limit <= 0:
        return None
    now = time.monotonic()
    window_start = now - 60
    with _send_lock:
        while _send_timestamps and _send_timestamps[0] < window_start:
            _send_timestamps.popleft()
        if len(_send_timestamps) >= limit:
            raise RuntimeError(
                f"Send rate limit exceeded ({limit}/min). Wait a moment and try again, "
                "or raise WHATSAPP_MCP_SEND_RATE_LIMIT."
            )
        _send_timestamps.append(now)
    return now


def _release_send_slot(reserved: Optional[float]) -> None:
    """Return a slot claimed by `_reserve_send_slot` after a send that never
    reached the bridge. Without this, an outage burns the whole minute's
    budget and the limiter then masks the real error for 60 seconds — even
    after the bridge is back up."""
    if reserved is None:
        return
    with _send_lock:
        try:
            _send_timestamps.remove(reserved)
        except ValueError:
            pass


def _validate_media_path(media_path: str) -> str:
    """Confirm `media_path` is an existing file and, when
    WHATSAPP_MCP_ALLOWED_MEDIA_DIRS is set, that it resolves inside one of
    those directories.

    Returns the caller's path, not the resolved one: the allowlist check has
    to follow symlinks (or it could be walked around), but the bridge derives
    the document title, MIME type and media class from the path string it is
    given, so sending the resolved target of a symlink would rename the file
    and mistype it for the recipient.
    """
    real_path = os.path.realpath(media_path)
    if not os.path.isfile(real_path):
        raise ValueError(f"Media file not found: {media_path}")

    allowed_dirs = allowed_media_dirs()
    if allowed_dirs is not None and not any(
        # Both sides are already realpaths, so a lexical check is exact.
        Path(real_path).is_relative_to(directory) for directory in allowed_dirs
    ):
        raise ValueError(
            f"Media file {media_path} is outside the allowed directories "
            f"({', '.join(allowed_dirs)}). Set WHATSAPP_MCP_ALLOWED_MEDIA_DIRS to permit it."
        )
    return media_path


def _post(
    endpoint: str, payload: Dict[str, Any], timeout: Tuple[int, int]
) -> Tuple[Optional[Dict[str, Any]], str]:
    """POST a payload to a bridge endpoint and decode its JSON body.

    Returns `(body, "")` whenever the bridge answered with JSON — it sends a
    {success, message} body on error responses too, so a non-200 still
    carries a usable message. Returns `(None, error)` when the request never
    completed or the response was not JSON (its plain-text validation errors).
    """
    try:
        response = requests.post(f"{api_base_url()}{endpoint}", json=payload, timeout=timeout)
    except (requests.Timeout, requests.ConnectionError) as e:
        return None, f"Could not reach the WhatsApp bridge ({BRIDGE_DOWN_HINT}): {e}"
    except requests.RequestException as e:
        return None, f"Request error: {str(e)}"
    except Exception as e:
        return None, f"Unexpected error: {str(e)}"

    try:
        return response.json(), ""
    except json.JSONDecodeError:
        return None, f"Error: HTTP {response.status_code} - {response.text}"


def post_limited(
    endpoint: str, payload: Dict[str, str], timeout: Tuple[float, float]
) -> Tuple[bool, str]:
    """Claim a rate-limit slot, POST to a bridge endpoint, and parse the
    {success, message} result."""
    reserved = _reserve_send_slot()
    body, error = _post(endpoint, payload, timeout=timeout)
    if body is None:
        # The request never reached the bridge, so it should not count
        # against the window.
        _release_send_slot(reserved)
        return False, error
    return body.get("success", False), body.get("message", "Unknown response")


def _post_send(payload: Dict[str, str]) -> Tuple[bool, str]:
    """Rate-limited POST to the bridge's /send endpoint.

    The read timeout is generous because it covers the bridge's whole upload
    to WhatsApp: timing out early would report a send as failed while it is
    still in flight, and the caller's retry would deliver it twice.
    """
    return post_limited("/send", payload, (5, _SEND_READ_TIMEOUT))


def send_message(recipient: str, message: str, reply_to: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        raise ValueError("Recipient must be provided")

    payload = {
        "recipient": recipient,
        "message": message,
    }
    if reply_to:
        payload["reply_to"] = reply_to
    return _post_send(payload)


def send_file(recipient: str, media_path: str, reply_to: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        raise ValueError("Recipient must be provided")
    if not media_path:
        raise ValueError("Media path must be provided")

    validated_path = _validate_media_path(media_path)
    payload = {
        "recipient": recipient,
        "media_path": validated_path
    }
    if reply_to:
        payload["reply_to"] = reply_to
    return _post_send(payload)


def send_audio_message(recipient: str, media_path: str, reply_to: Optional[str] = None) -> Tuple[bool, str]:
    if not recipient:
        raise ValueError("Recipient must be provided")
    if not media_path:
        raise ValueError("Media path must be provided")

    validated_path = _validate_media_path(media_path)

    temp_path = None
    send_path = validated_path
    if not validated_path.endswith(".ogg"):
        try:
            temp_path = audio.convert_to_opus_ogg_temp(validated_path)
        except Exception as e:
            raise RuntimeError(
                f"Error converting file to opus ogg. You likely need to install ffmpeg: {e}"
            ) from e
        send_path = temp_path

    try:
        payload = {
            "recipient": recipient,
            "media_path": send_path
        }
        if reply_to:
            payload["reply_to"] = reply_to
        return _post_send(payload)
    finally:
        if temp_path:
            try:
                os.unlink(temp_path)
            except OSError as e:
                logger.warning(f"Failed to clean up temp ogg file {temp_path}: {e}")


def download_media(message_id: str, chat_jid: str) -> Tuple[Optional[str], str]:
    """Download media from a message and return the local file path.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        (path, message): path is the local file path on success, None on
        failure; message always carries the bridge's (or transport-layer)
        status text.
    """
    body, error = _post(
        "/download", {"message_id": message_id, "chat_jid": chat_jid}, timeout=(5, 120)
    )
    if body is None:
        logger.error(error)
        return None, error

    if not body.get("success", False):
        message = body.get("message", "Unknown error")
        logger.warning(f"Download failed: {message}")
        return None, message

    path = body.get("path")
    message = body.get("message", "Media downloaded successfully")
    logger.info(f"Media downloaded successfully: {path}")
    return path, message


def get_group_members(group_jid: str) -> dict:
    """Get members of a WhatsApp group.

    Args:
        group_jid: The JID of the group (must end with @g.us)

    Returns:
        A dictionary containing success status, group name, member count, and list of members
    """
    body, error = _post("/group/members", {"group_jid": group_jid}, timeout=(5, 30))
    if body is None:
        return {"success": False, "message": error, "members": [], "member_count": 0}
    return body
