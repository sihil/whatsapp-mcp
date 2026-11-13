import sqlite3

import pytest

from conftest import GROUP
from whatsapp import download_media, format_message, list_messages

_MEDIA = [
    # id, content (caption), timestamp, media_type, filename, file_length, mime_type
    ("doc1", "", "2026-11-06 08:00:00+00:00", "document", "Split Plan.pdf", 440388, "application/pdf"),
    ("img1", "the rota", "2026-11-06 09:00:00+00:00", "image", "image_20260712_203448.jpg", 91234, "image/jpeg"),
]


def _add_media(path, with_mime_column=True):
    conn = sqlite3.connect(path)
    if with_mime_column:
        conn.execute("ALTER TABLE messages ADD COLUMN mime_type TEXT")
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, "
            "media_type, filename, file_length, mime_type) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
            [(i, GROUP, GROUP, c, ts, mt, fn, size, mime) for i, c, ts, mt, fn, size, mime in _MEDIA],
        )
    else:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, "
            "media_type, filename, file_length) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)",
            [(i, GROUP, GROUP, c, ts, mt, fn, size) for i, c, ts, mt, fn, size, _ in _MEDIA],
        )
    conn.commit()
    conn.close()


def _by_id(messages):
    return {m["id"]: m for m in messages}


def test_document_listing_carries_filename_size_and_mime_type(store):
    _add_media(store)
    doc = _by_id(list_messages(chat_jid=GROUP, include_context=False))["doc1"]
    assert doc["media_type"] == "document"
    assert doc["filename"] == "Split Plan.pdf"
    assert doc["size"] == 440388
    assert doc["mime_type"] == "application/pdf"


def test_image_listing_hides_generated_filename(store):
    _add_media(store)
    img = _by_id(list_messages(chat_jid=GROUP, include_context=False))["img1"]
    assert img["filename"] is None
    assert img["mime_type"] == "image/jpeg"
    assert img["size"] == 91234
    assert img["content"] == "the rota"


def test_text_message_has_no_media_fields(store):
    text = _by_id(list_messages(chat_jid=GROUP, include_context=False))["g1"]
    assert "filename" not in text and "size" not in text and "mime_type" not in text


def test_store_without_mime_column_reads_as_unknown(store):
    _add_media(store, with_mime_column=False)
    doc = _by_id(list_messages(chat_jid=GROUP, include_context=False))["doc1"]
    assert doc["mime_type"] is None
    assert doc["filename"] == "Split Plan.pdf"


def test_formatted_line_names_the_document(store):
    _add_media(store)
    from db import connect, message_from_row
    from db_names import message_source

    conn = connect()
    cursor = conn.cursor()
    source = message_source(cursor)
    cursor.execute(f"{source} WHERE messages.id = 'doc1'")
    line = format_message(message_from_row(cursor.fetchone()), cursor=cursor)
    conn.close()
    assert "Split Plan.pdf" in line
    assert "application/pdf" in line
    assert "430 KB" in line


def test_download_reports_mime_type_and_size(store, bridge, monkeypatch):
    import requests

    from conftest import FakeResponse

    monkeypatch.setattr(
        requests, "post",
        lambda url, json=None, timeout=None: FakeResponse({
            "success": True, "message": "ok", "path": "/tmp/x.pdf",
            "filename": "Split Plan.pdf", "mime_type": "application/pdf", "size": 440388,
        }),
    )
    path, _, details = download_media("doc1", GROUP)
    assert path == "/tmp/x.pdf"
    assert details == {"filename": "Split Plan.pdf", "mime_type": "application/pdf", "size": 440388}


@pytest.mark.parametrize("size,text", [(999, "999 B"), (440388, "430 KB"), (5_400_000, "5.1 MB")])
def test_size_is_human_readable(size, text):
    from db_chats import human_size

    assert human_size(size) == text
