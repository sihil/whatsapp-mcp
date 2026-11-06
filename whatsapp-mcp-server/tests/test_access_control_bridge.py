import pytest

from conftest import FRIEND, GROUP, OTHER_GROUP
from whatsapp import (
    delete_message,
    download_media,
    edit_message,
    get_group_members,
    send_file,
    send_message,
    send_reaction,
)


def test_send_message_to_writable_chat_reaches_bridge(store, bridge):
    assert send_message(GROUP, "hi") == (True, "ok")
    assert len(bridge) == 1


@pytest.mark.parametrize("recipient", [OTHER_GROUP, FRIEND, "447700900001"])
def test_send_message_to_unwritable_recipient_is_refused(store, bridge, recipient):
    with pytest.raises(PermissionError):
        send_message(recipient, "hi")
    assert bridge == []


def test_send_file_to_unwritable_recipient_is_refused(store, bridge, tmp_path):
    media = tmp_path / "photo.jpg"
    media.write_bytes(b"x")
    with pytest.raises(PermissionError):
        send_file(OTHER_GROUP, str(media))
    assert bridge == []


@pytest.mark.parametrize(
    "action",
    [
        lambda: send_reaction(OTHER_GROUP, "s1", "👍"),
        lambda: edit_message(OTHER_GROUP, "s1", "new"),
        lambda: delete_message(OTHER_GROUP, "s1"),
    ],
)
def test_message_actions_on_unwritable_chat_are_refused(store, bridge, action):
    with pytest.raises(PermissionError):
        action()
    assert bridge == []


def test_message_action_on_writable_chat_reaches_bridge(store, bridge):
    assert send_reaction(GROUP, "g1", "👍") == (True, "ok")
    assert len(bridge) == 1


def test_download_media_from_unreadable_chat_is_refused(store, bridge):
    with pytest.raises(PermissionError):
        download_media("s1", OTHER_GROUP)
    assert bridge == []


def test_download_media_from_readable_chat_reaches_bridge(store, bridge):
    assert download_media("f1", FRIEND)[0] == "/tmp/x"


def test_group_members_of_unreadable_group_is_refused(store, bridge):
    with pytest.raises(PermissionError):
        get_group_members(OTHER_GROUP)
    assert bridge == []


def test_group_members_of_readable_group_reaches_bridge(store, bridge):
    assert get_group_members(GROUP)["success"] is True
    assert bridge[0][1] == {"group_jid": GROUP}
