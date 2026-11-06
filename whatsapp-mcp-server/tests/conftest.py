import json
import os
import sqlite3
import sys
import tempfile

import pytest

GROUP = "120363000000000001@g.us"
OTHER_GROUP = "120363000000000002@g.us"
FRIEND = "447700900001@s.whatsapp.net"
FRIEND_LID = "11111111111111@lid"
STRANGER = "447700900002@s.whatsapp.net"

# access_control reads its config once, at import, so the allowlist has to be
# in place before any server module is imported. FRIEND is listed only by LID
# to exercise jid_aliases expansion.
_allowlist = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
json.dump(
    {
        "name": "test",
        "allowRead": [{"id": GROUP}, {"id": FRIEND_LID}],
        "allowWrite": [{"id": GROUP}],
    },
    _allowlist,
)
_allowlist.close()
os.environ["WHATSAPP_MCP_ALLOWLIST"] = _allowlist.name
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

_SCHEMA = """
    CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
    CREATE TABLE messages (
        id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
        is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT,
        media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
        PRIMARY KEY (id, chat_jid)
    );
    CREATE TABLE jid_aliases (old_jid TEXT PRIMARY KEY, new_jid TEXT NOT NULL);
    CREATE TABLE contacts (
        jid TEXT PRIMARY KEY, full_name TEXT NOT NULL DEFAULT '',
        push_name TEXT NOT NULL DEFAULT '', business_name TEXT NOT NULL DEFAULT ''
    );
"""

_CHATS = [
    (GROUP, "Allowed group", "2026-11-01 10:00:00+00:00"),
    (OTHER_GROUP, "Secret group", "2026-11-02 10:00:00+00:00"),
    (FRIEND, "Friend", "2026-11-03 10:00:00+00:00"),
    (STRANGER, "Stranger", "2026-11-04 10:00:00+00:00"),
]

_MESSAGES = [
    ("g1", GROUP, FRIEND, "hello group", "2026-11-01 10:00:00+00:00"),
    ("s1", OTHER_GROUP, FRIEND, "secret hello", "2026-11-02 10:00:00+00:00"),
    ("f1", FRIEND, FRIEND, "hello friend", "2026-11-03 10:00:00+00:00"),
    ("x1", STRANGER, STRANGER, "hello stranger", "2026-11-04 10:00:00+00:00"),
]


@pytest.fixture
def store(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(_SCHEMA)
    conn.executemany("INSERT INTO chats VALUES (?, ?, ?)", _CHATS)
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
        "VALUES (?, ?, ?, ?, ?, 0)",
        _MESSAGES,
    )
    conn.execute("INSERT INTO jid_aliases VALUES (?, ?)", (FRIEND_LID, FRIEND))
    conn.executemany(
        "INSERT INTO contacts (jid, full_name) VALUES (?, ?)",
        [(FRIEND, "Friend Person"), (STRANGER, "Stranger Person")],
    )
    conn.commit()
    conn.close()
    monkeypatch.setenv("WHATSAPP_MCP_DB_PATH", str(path))
    return path


class FakeResponse:
    status_code = 200

    def __init__(self, body):
        self._body = body

    def json(self):
        return self._body


@pytest.fixture
def bridge(monkeypatch):
    """Records POSTs to the bridge instead of sending them."""
    import requests

    calls = []

    def fake_post(url, json=None, timeout=None):
        calls.append((url, json))
        return FakeResponse({"success": True, "message": "ok", "members": [], "path": "/tmp/x"})

    monkeypatch.setattr(requests, "post", fake_post)
    return calls
