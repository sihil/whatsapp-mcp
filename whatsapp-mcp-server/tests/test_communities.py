import sqlite3

import pytest

from conftest import COMMUNITY, GROUP, OTHER_GROUP, PLAIN_COMMUNITY
from whatsapp import get_chat, get_community, list_chats, list_communities

ANNOUNCE = "120363000000000101@g.us"
SUBGROUP = "120363000000000102@g.us"
OTHER_COMMUNITY = "120363000000000300@g.us"
SIBLING = "120363000000000301@g.us"
PLAIN_SUB = "120363000000000201@g.us"

_LINKS = [
    (COMMUNITY, "", 1, 0, "Edinburgh"),
    (ANNOUNCE, COMMUNITY, 0, 1, "Edinburgh announcements"),
    (SUBGROUP, COMMUNITY, 0, 0, "Corstorphine"),
    (OTHER_COMMUNITY, "", 1, 0, "Glasgow"),
    (GROUP, OTHER_COMMUNITY, 0, 0, "Allowed group"),
    (SIBLING, OTHER_COMMUNITY, 0, 0, "Glasgow sibling"),
    (PLAIN_COMMUNITY, "", 1, 0, "Fife"),
    (PLAIN_SUB, PLAIN_COMMUNITY, 0, 0, "Fife subgroup"),
]


@pytest.fixture
def communities(store):
    conn = sqlite3.connect(store)
    conn.executemany(
        "INSERT INTO group_links (group_jid, parent_jid, is_community, is_announcement, name) "
        "VALUES (?, ?, ?, ?, ?)",
        _LINKS,
    )
    conn.executemany("UPDATE group_links SET is_member = 1 WHERE group_jid = ?", [(ANNOUNCE,), (GROUP,)])
    conn.executemany(
        "INSERT INTO chats VALUES (?, ?, ?)",
        [
            (SUBGROUP, "Corstorphine", "2026-11-07 10:00:00+00:00"),
            (SIBLING, "Glasgow sibling", "2026-11-07 11:00:00+00:00"),
        ],
    )
    conn.commit()
    conn.close()
    return store


def _link(store, group, parent):
    conn = sqlite3.connect(store)
    conn.execute(
        "INSERT OR REPLACE INTO group_links (group_jid, parent_jid) VALUES (?, ?)", (group, parent)
    )
    conn.commit()
    conn.close()


def test_community_entry_covers_its_linked_groups(communities):
    assert {c["jid"] for c in list_chats()} >= {SUBGROUP}
    assert get_chat(SUBGROUP)["name"] == "Corstorphine"


def test_group_linked_later_is_readable_without_a_restart(communities):
    with pytest.raises(PermissionError):
        get_chat(OTHER_GROUP)
    _link(communities, OTHER_GROUP, COMMUNITY)
    assert get_chat(OTHER_GROUP)["jid"] == OTHER_GROUP


def test_plain_group_entry_does_not_cover_its_siblings(communities):
    with pytest.raises(PermissionError):
        get_chat(SIBLING)


def test_plain_community_entry_does_not_cover_its_subgroups(communities):
    _link(communities, OTHER_GROUP, PLAIN_COMMUNITY)
    with pytest.raises(PermissionError):
        get_chat(OTHER_GROUP)


def test_store_without_group_links_covers_only_the_community(store):
    conn = sqlite3.connect(store)
    conn.execute("DROP TABLE group_links")
    conn.close()
    assert {c["jid"] for c in list_chats()} == {GROUP, "447700900001@s.whatsapp.net"}


def _by_jid(items):
    return {item["jid"]: item for item in items}


def test_list_communities_shows_readable_subgroups_and_counts_the_rest(communities):
    found = _by_jid(list_communities())
    assert set(found) == {COMMUNITY, OTHER_COMMUNITY, PLAIN_COMMUNITY}

    edinburgh = found[COMMUNITY]
    assert edinburgh["name"] == "Edinburgh"
    assert edinburgh["readable"] is True
    assert edinburgh["hidden_subgroups"] == 0
    subgroups = _by_jid(edinburgh["subgroups"])
    assert set(subgroups) == {ANNOUNCE, SUBGROUP}
    assert subgroups[ANNOUNCE]["is_announcement"] is True
    assert subgroups[SUBGROUP]["last_message_time"] == "2026-11-07T10:00:00+00:00"

    glasgow = found[OTHER_COMMUNITY]
    assert glasgow["readable"] is False
    assert [s["jid"] for s in glasgow["subgroups"]] == [GROUP]
    assert glasgow["hidden_subgroups"] == 1

    assert found[PLAIN_COMMUNITY]["hidden_subgroups"] == 1


def test_get_community_of_unreadable_community_is_refused(communities):
    with pytest.raises(PermissionError):
        get_community(OTHER_COMMUNITY)


def test_get_community_of_a_plain_group_is_an_error(communities):
    with pytest.raises(ValueError, match="not a community"):
        get_community(SUBGROUP)


def test_get_community_members_come_from_the_bridge(communities, bridge):
    result = get_community(COMMUNITY, include_members=True)
    assert {s["jid"] for s in result["subgroups"]} == {ANNOUNCE, SUBGROUP}
    assert bridge[0][1] == {"community_jid": COMMUNITY}
    assert result["members"] == []


def test_get_community_members_refused_while_subgroups_are_hidden(communities, bridge):
    with pytest.raises(PermissionError, match="1 of its groups"):
        get_community(PLAIN_COMMUNITY, include_members=True)
    assert bridge == []


def test_community_tools_are_registered(communities):
    import asyncio

    from main import mcp

    listed = asyncio.run(mcp.call_tool("list_communities", {}))
    one = asyncio.run(mcp.call_tool("get_community", {"community_jid": COMMUNITY}))
    assert len(listed.structured_content["result"]) == 3
    assert one.structured_content["result"]["jid"] == COMMUNITY


def test_subgroups_say_whether_the_account_is_in_them(communities):
    edinburgh = _by_jid(_by_jid(list_communities())[COMMUNITY]["subgroups"])
    assert edinburgh[ANNOUNCE]["is_member"] is True
    assert edinburgh[SUBGROUP]["is_member"] is False


def test_membership_is_unknown_on_a_store_without_it(communities):
    conn = sqlite3.connect(communities)
    conn.execute("ALTER TABLE group_links DROP COLUMN is_member")
    conn.close()
    subgroups = _by_jid(list_communities())[COMMUNITY]["subgroups"]
    assert {s["is_member"] for s in subgroups} == {None}
