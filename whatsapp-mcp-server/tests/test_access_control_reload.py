import json
import os
import time

import pytest

import access_control
from conftest import GROUP, OTHER_GROUP, STRANGER
from whatsapp import get_chat, list_chats, send_message


@pytest.fixture
def allowlist():
    """Rewrite the session's allowlist file in place, restoring it afterwards."""
    path = os.environ["WHATSAPP_MCP_ALLOWLIST"]
    with open(path) as f:
        original = f.read()

    def write(content):
        with open(path, "w") as f:
            f.write(content if isinstance(content, str) else json.dumps(content))
        # Coarse filesystem timestamps could hide a rewrite within one tick.
        stamp = time.time_ns() + 1_000_000_000 * write.calls
        write.calls += 1
        os.utime(path, ns=(stamp, stamp))

    write.calls = 1
    yield write
    write(original)


def _config(read, write=(), name="test"):
    return {
        "name": name,
        "allowRead": [{"id": jid} for jid in read],
        "allowWrite": [{"id": jid} for jid in write],
    }


def test_added_read_entry_applies_without_restart(store, allowlist):
    with pytest.raises(PermissionError):
        get_chat(OTHER_GROUP)
    allowlist(_config([GROUP, OTHER_GROUP]))
    assert get_chat(OTHER_GROUP)["jid"] == OTHER_GROUP


def test_removed_read_entry_is_revoked_without_restart(store, allowlist):
    allowlist(_config([OTHER_GROUP]))
    assert {c["jid"] for c in list_chats()} == {OTHER_GROUP}
    with pytest.raises(PermissionError):
        get_chat(GROUP)


def test_added_write_entry_applies_without_restart(store, bridge, allowlist):
    allowlist(_config([GROUP], write=[GROUP, STRANGER]))
    assert send_message(STRANGER, "hi")[0] is True


def test_renamed_instance_appears_in_errors(store, allowlist):
    allowlist(_config([GROUP], name="renamed"))
    with pytest.raises(PermissionError, match="'renamed'"):
        get_chat(OTHER_GROUP)


def test_malformed_edit_denies_everything_until_fixed(store, allowlist):
    allowlist("{ not json")
    with pytest.raises(PermissionError, match="allowlist config"):
        get_chat(GROUP)
    with pytest.raises(PermissionError, match="allowlist config"):
        list_chats()
    with pytest.raises(PermissionError, match="allowlist config"):
        access_control.check_writable(GROUP)

    allowlist(_config([GROUP]))
    assert get_chat(GROUP)["jid"] == GROUP


def test_invalid_edit_denies_everything(store, allowlist):
    allowlist({"allowRead": [], "allowWrite": []})
    with pytest.raises(PermissionError, match="allowRead"):
        get_chat(GROUP)


def test_deleted_file_denies_everything(store, allowlist):
    os.remove(os.environ["WHATSAPP_MCP_ALLOWLIST"])
    with pytest.raises(PermissionError, match="allowlist config"):
        get_chat(GROUP)
