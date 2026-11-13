import os
import sqlite3

import pytest
import requests

from conftest import GROUP, FakeResponse

import tools_send

DOC_ID = "3B60F24714E2304DBB99"


@pytest.fixture
def media_store(store):
    conn = sqlite3.connect(store)
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) "
        "VALUES (?, ?, ?, '', ?, 0, ?, ?)",
        [
            (DOC_ID, GROUP, GROUP, "2026-11-06 08:25:57+00:00", "document", "Split Plan.pdf"),
            ("IMG1", GROUP, GROUP, "2026-11-06 09:01:00+00:00", "image", "image_20260712_203448.jpg"),
        ],
    )
    conn.execute("UPDATE chats SET name = 'Allowed / group: 1' WHERE jid = ?", (GROUP,))
    conn.commit()
    conn.close()
    return store


@pytest.fixture
def bridge_file(tmp_path, monkeypatch):
    """A file the fake bridge reports as downloaded, with configurable details."""
    src = tmp_path / "bridge" / "cached.bin"
    src.parent.mkdir()
    src.write_bytes(b"%PDF-1.4 test")
    reply = {"success": True, "message": "ok", "path": str(src),
             "filename": "Split Plan.pdf", "mime_type": "application/pdf", "size": 13}
    monkeypatch.setattr(requests, "post", lambda url, json=None, timeout=None: FakeResponse(reply))
    return reply


def _download(message_id=DOC_ID):
    return tools_send.download_media(message_id, GROUP)


def test_without_media_dir_returns_bridge_path(media_store, bridge_file, monkeypatch):
    monkeypatch.delenv("WHATSAPP_MCP_MEDIA_DIR", raising=False)
    result = _download()
    assert result["file_path"] == bridge_file["path"]
    assert "media_dir_path" not in result


def test_copies_into_media_dir_by_chat_and_time(media_store, bridge_file, tmp_path, monkeypatch):
    media_dir = tmp_path / "attachments" / "sfc"
    monkeypatch.setenv("WHATSAPP_MCP_MEDIA_DIR", str(media_dir))
    result = _download()
    rel = result["media_dir_path"]
    assert rel == os.path.join("Allowed _ group_ 1", "2026-11-06 0825 Split Plan.pdf")
    saved = media_dir / rel
    assert saved.read_bytes() == b"%PDF-1.4 test"
    assert result["file_path"] == str(saved)
    assert result["media_dir"] == str(media_dir)


def test_non_document_is_named_by_type_and_id(media_store, bridge_file, tmp_path, monkeypatch):
    bridge_file.update(filename="image_20260712_203448.jpg", mime_type="image/jpeg")
    monkeypatch.setenv("WHATSAPP_MCP_MEDIA_DIR", str(tmp_path / "m"))
    rel = _download("IMG1")["media_dir_path"]
    assert os.path.basename(rel) == "2026-11-06 0901 image IMG1.jpg"


def test_repeat_download_reuses_the_copy(media_store, bridge_file, tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_MCP_MEDIA_DIR", str(tmp_path / "m"))
    first = _download()["media_dir_path"]
    assert _download()["media_dir_path"] == first
    assert len(list((tmp_path / "m").rglob("*"))) == 2  # chat folder + file


def test_different_file_with_same_name_gets_the_message_id(media_store, bridge_file, tmp_path, monkeypatch):
    media_dir = tmp_path / "m"
    monkeypatch.setenv("WHATSAPP_MCP_MEDIA_DIR", str(media_dir))
    first = _download()["media_dir_path"]
    (media_dir / first).write_bytes(b"something else entirely")
    second = _download()["media_dir_path"]
    assert second != first
    assert DOC_ID in second


def test_sender_filename_cannot_escape_media_dir(media_store, bridge_file, tmp_path, monkeypatch):
    conn = sqlite3.connect(media_store)
    conn.execute("UPDATE messages SET filename = '../../../evil.sh' WHERE id = ?", (DOC_ID,))
    conn.commit()
    conn.close()
    media_dir = tmp_path / "m"
    monkeypatch.setenv("WHATSAPP_MCP_MEDIA_DIR", str(media_dir))
    saved = _download()["file_path"]
    assert os.path.realpath(saved).startswith(os.path.realpath(media_dir) + os.sep)
