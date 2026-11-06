import pytest

from conftest import FRIEND, FRIEND_LID, GROUP, OTHER_GROUP, STRANGER
from whatsapp import (
    get_chat,
    get_contact_chats,
    get_direct_chat_by_contact,
    get_last_interaction,
    get_message_context,
    list_chats,
    list_messages,
    search_contacts,
)


def _jids(rows):
    return {row["jid"] for row in rows}


def test_list_chats_returns_only_readable_chats(store):
    assert _jids(list_chats()) == {GROUP, FRIEND}


def test_list_chats_query_cannot_find_unreadable_chat(store):
    assert list_chats(query="Secret") == []


def test_list_chats_without_last_message(store):
    assert _jids(list_chats(include_last_message=False)) == {GROUP, FRIEND}


def test_get_chat_refuses_unreadable_chat(store):
    with pytest.raises(PermissionError):
        get_chat(OTHER_GROUP)


def test_get_chat_follows_alias_of_allowlisted_lid(store):
    assert get_chat(FRIEND)["jid"] == FRIEND
    assert get_chat(FRIEND_LID)["jid"] == FRIEND


def test_list_messages_returns_only_readable_chats(store):
    messages = list_messages(include_context=False)
    assert {m["id"] for m in messages} == {"g1", "f1"}


def test_list_messages_in_unreadable_chat_is_refused(store):
    with pytest.raises(PermissionError):
        list_messages(chat_jid=OTHER_GROUP)


def test_list_messages_by_sender_omits_unreadable_chats(store):
    messages = list_messages(sender_phone_number="447700900001", include_context=False)
    assert {m["id"] for m in messages} == {"g1", "f1"}


def test_get_message_context_hides_unreadable_message(store):
    with pytest.raises(ValueError, match="not found"):
        get_message_context("s1")


def test_get_message_context_in_unreadable_chat_is_refused(store):
    with pytest.raises(PermissionError):
        get_message_context("s1", chat_jid=OTHER_GROUP)


def test_search_contacts_returns_only_readable_contacts(store):
    assert _jids(search_contacts("r")) == {FRIEND}


def test_get_contact_chats_returns_only_readable_chats(store):
    assert _jids(get_contact_chats(FRIEND)) == {GROUP, FRIEND}


def test_get_last_interaction_ignores_unreadable_chats(store):
    assert "hello friend" in get_last_interaction(FRIEND)


def test_get_last_interaction_with_unreadable_contact_finds_nothing(store):
    with pytest.raises(ValueError, match="No messages"):
        get_last_interaction(STRANGER)


def test_get_direct_chat_by_contact_refuses_unreadable_contact(store):
    with pytest.raises(ValueError, match="No direct chat"):
        get_direct_chat_by_contact("447700900002")


def test_get_direct_chat_by_contact_finds_readable_contact(store):
    assert get_direct_chat_by_contact("447700900001")["jid"] == FRIEND
