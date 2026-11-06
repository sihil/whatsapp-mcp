import sqlite3

import pytest

from conftest import FRIEND, GROUP, OTHER_GROUP, STRANGER
from whatsapp import list_group_events, list_reactions


def test_list_reactions_newest_first_in_readable_chats(store):
    reactions = list_reactions()
    assert [(r["emoji"], r["chat_jid"]) for r in reactions] == [("❤️", FRIEND), ("👍", GROUP)]


def test_list_reactions_names_the_reactor_and_the_message(store):
    reaction = list_reactions(chat_jid=GROUP)[0]
    assert reaction["sender"] == STRANGER
    assert reaction["sender_name"] == "Stranger Person"
    assert reaction["chat_name"] == "Allowed group"
    assert reaction["message"]["id"] == "g1"
    assert reaction["message"]["content"] == "hello group"
    assert reaction["message"]["sender_name"] == "Friend Person"


def test_list_reactions_after(store):
    assert [r["emoji"] for r in list_reactions(after="2026-11-05T09:30:00+00:00")] == ["❤️"]


def test_list_reactions_in_unreadable_chat_is_refused(store):
    with pytest.raises(PermissionError):
        list_reactions(chat_jid=OTHER_GROUP)


def test_list_group_events_newest_first_in_readable_chats(store):
    events = list_group_events()
    assert [(e["kind"], e["participant"]) for e in events] == [("leave", FRIEND), ("join", STRANGER)]


def test_list_group_events_names_participant_and_actor(store):
    join = list_group_events(chat_jid=GROUP, before="2026-11-06T00:00:00+00:00")[0]
    assert join == {
        "chat_jid": GROUP,
        "chat_name": "Allowed group",
        "kind": "join",
        "participant": STRANGER,
        "participant_name": "Stranger Person",
        "actor": FRIEND,
        "actor_name": "Friend Person",
        "reason": "added",
        "timestamp": "2026-11-05T08:00:00+00:00",
    }


def test_list_group_events_in_unreadable_chat_is_refused(store):
    with pytest.raises(PermissionError):
        list_group_events(chat_jid=OTHER_GROUP)


def test_store_from_an_older_bridge_has_no_activity(store):
    conn = sqlite3.connect(store)
    conn.executescript("DROP TABLE reactions; DROP TABLE group_events;")
    conn.close()
    assert list_reactions() == []
    assert list_group_events() == []


def test_activity_tools_are_registered(store):
    import asyncio

    from main import mcp

    reactions = asyncio.run(mcp.call_tool("list_reactions", {"chat_jid": GROUP}))
    events = asyncio.run(mcp.call_tool("list_group_events", {"chat_jid": GROUP}))
    assert reactions.structured_content["result"][0]["emoji"] == "👍"
    assert [e["kind"] for e in events.structured_content["result"]] == ["leave", "join"]
