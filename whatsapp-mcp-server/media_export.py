"""Copies downloaded media into WHATSAPP_MCP_MEDIA_DIR.

The bridge caches downloads under its own store, which a Claude Desktop
session cannot open. A session can open a folder mounted into it, so when the
setting names one, download_media also puts the file there, laid out for
browsing: <dir>/<chat name>/<YYYY-MM-DD HHMM> <name>.
"""
import filecmp
import os
import shutil
from datetime import datetime
from typing import Optional

import access_control
from config import media_dir
from db import connect

_UNSAFE = str.maketrans({"/": "_", "\\": "_", ":": "_", "\0": "_"})


def _safe(name: str) -> str:
    """One path component: no separators, and never '.', '..' or hidden."""
    name = os.path.basename(name.translate(_UNSAFE)).strip().lstrip(".")
    return name or "_"


def _message_facts(message_id: str, chat_jid: str):
    """(timestamp, chat name, media type, stored filename) for a readable
    message, preferring the chat the caller named."""
    conn = connect()
    try:
        cursor = conn.cursor()
        where, params = access_control.read_filter(cursor, "messages.chat_jid")
        cursor.execute(
            f"""SELECT messages.timestamp, chats.name, messages.chat_jid,
                       messages.media_type, messages.filename
                FROM messages JOIN chats ON messages.chat_jid = chats.jid
                WHERE messages.id = ? AND {where}
                ORDER BY messages.chat_jid = ? DESC LIMIT 1""",
            (message_id, *params, chat_jid),
        )
        return cursor.fetchone()
    finally:
        conn.close()


def export_media(src: str, message_id: str, chat_jid: str, bridge_filename: Optional[str]) -> Optional[str]:
    """Copy the bridge's file into the media directory and return the copy's
    path relative to it, or None when no media directory is configured."""
    root = media_dir()
    if root is None:
        return None
    facts = _message_facts(message_id, chat_jid)
    if facts is None:
        raise ValueError(f"Message {message_id} not found in {chat_jid}")
    timestamp, chat_name, stored_chat, media_type, stored_filename = facts

    stamp = datetime.fromisoformat(timestamp).strftime("%Y-%m-%d %H%M")
    if media_type == "document" and stored_filename:
        name = _safe(stored_filename)
    else:
        ext = os.path.splitext(bridge_filename or stored_filename or "")[1]
        name = _safe(f"{media_type or 'media'} {message_id}{ext}")
    folder = _safe(chat_name or stored_chat.split("@")[0])

    rel = os.path.join(folder, f"{stamp} {name}")
    dest = os.path.join(root, rel)
    if os.path.exists(dest) and not filecmp.cmp(src, dest, shallow=False):
        base, ext = os.path.splitext(name)
        rel = os.path.join(folder, f"{stamp} {base} ({_safe(message_id)}){ext}")
        dest = os.path.join(root, rel)

    if not (os.path.exists(dest) and filecmp.cmp(src, dest, shallow=False)):
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        tmp = dest + ".part"
        shutil.copyfile(src, tmp)
        os.replace(tmp, dest)
    return rel
