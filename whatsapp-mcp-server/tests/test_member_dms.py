import sqlite3

import pytest

import access_control
from access_report import member_dm_report
from conftest import COMMUNITY, FRIEND, FRIEND_LID, GROUP, OTHER_GROUP
from whatsapp import get_chat, list_chats, list_messages

ME = "447700900099@s.whatsapp.net"
SFC_ONLY = "447700900010@s.whatsapp.net"      # in GROUP only, not saved
SFC_ONLY_LID = "22222222222222@lid"
SAVED = "447700900011@s.whatsapp.net"         # in GROUP only, in the address book
BOTH = "447700900012@s.whatsapp.net"          # in GROUP and OTHER_GROUP
OUTSIDER = "447700900013@s.whatsapp.net"      # in OTHER_GROUP only
COMMUNITY_ONLY = "447700900014@s.whatsapp.net"  # only in the community-wide list

PEOPLE = [SFC_ONLY, SAVED, BOTH, OUTSIDER, COMMUNITY_ONLY]


@pytest.fixture
def members(store):
    conn = sqlite3.connect(store)
    conn.executemany(
        "INSERT INTO group_participants VALUES (?, ?)",
        [(GROUP, SFC_ONLY), (GROUP, SAVED), (GROUP, BOTH), (OTHER_GROUP, BOTH),
         (OTHER_GROUP, OUTSIDER), (COMMUNITY, COMMUNITY_ONLY), (GROUP, ME),
         (GROUP, FRIEND), (OTHER_GROUP, FRIEND)],
    )
    for i, jid in enumerate(PEOPLE):
        conn.execute("INSERT INTO chats VALUES (?, ?, ?)", (jid, f"Person {i}", "2026-11-20 10:00:00+00:00"))
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
            "VALUES (?, ?, ?, 'hi', '2026-11-20 10:00:00+00:00', 0)",
            (f"dm{i}", jid, jid),
        )
    conn.execute("INSERT INTO chats VALUES (?, 'Me', '2026-11-20 10:00:00+00:00')", (ME,))
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
        "VALUES ('me1', ?, ?, 'note to self', '2026-11-20 10:00:00+00:00', 1)", (ME, ME),
    )
    conn.execute("INSERT INTO contacts (jid, full_name) VALUES (?, 'Saved Friend')", (SAVED,))
    conn.execute("INSERT INTO jid_aliases VALUES (?, ?)", (SFC_ONLY_LID, SFC_ONLY))
    conn.commit()
    conn.close()
    return store


def _config(member_dms=True, deny=(), **extra):
    config = {
        "name": "test",
        "allowRead": [
            {"id": GROUP},
            {"id": FRIEND_LID},
            {"id": COMMUNITY, "include": "community"},
        ],
        "allowWrite": [{"id": GROUP}],
        "denyRead": [{"id": jid} for jid in deny],
        **extra,
    }
    if member_dms is not None:
        config["memberDMs"] = member_dms
    return config


def _readable(jid):
    try:
        access_control.check_readable(jid)
        return True
    except PermissionError:
        return False


def test_off_by_default(members, allowlist):
    allowlist(_config(member_dms=None))
    assert not _readable(SFC_ONLY)


def test_reads_dms_with_people_only_in_readable_groups(members, allowlist):
    allowlist(_config())
    assert get_chat(SFC_ONLY)["jid"] == SFC_ONLY
    assert SFC_ONLY in {c["jid"] for c in list_chats(limit=100)}
    assert "dm0" in {m["id"] for m in list_messages(chat_jid=SFC_ONLY, include_context=False)}


def test_excludes_address_book_other_groups_and_outsiders(members, allowlist):
    allowlist(_config())
    assert not _readable(SAVED)
    assert not _readable(BOTH)
    assert not _readable(OUTSIDER)
    assert OUTSIDER not in {c["jid"] for c in list_chats(limit=100)}


def test_community_wide_members_count(members, allowlist):
    allowlist(_config())
    assert _readable(COMMUNITY_ONLY)


def test_own_chat_is_never_admitted(members, allowlist):
    allowlist(_config())
    assert not _readable(ME)


def test_follows_aliases(members, allowlist):
    allowlist(_config())
    assert _readable(SFC_ONLY_LID)


def test_deny_overrides_the_rule_and_explicit_entries(members, allowlist):
    allowlist(_config(deny=[SFC_ONLY, FRIEND]))
    assert not _readable(SFC_ONLY)
    assert not _readable(SFC_ONLY_LID)
    assert not _readable(FRIEND_LID)
    assert _readable(COMMUNITY_ONLY)


def test_writes_are_unaffected(members, allowlist):
    allowlist(_config())
    with pytest.raises(PermissionError):
        access_control.check_writable(SFC_ONLY)


def test_non_boolean_flag_is_a_config_error(members, allowlist):
    allowlist(_config(member_dms="yes"))
    with pytest.raises(PermissionError, match="memberDMs"):
        get_chat(GROUP)


def test_store_without_members_table_admits_nobody(members, allowlist):
    conn = sqlite3.connect(members)
    conn.execute("DROP TABLE group_participants")
    conn.commit()
    conn.close()
    allowlist(_config())
    assert not _readable(SFC_ONLY)
    assert _readable(GROUP)


def test_report_explains_each_decision(members, allowlist):
    allowlist(_config(deny=[COMMUNITY_ONLY]))
    report = member_dm_report()
    admitted = {r["jid"] for r in report["admitted"]}
    excluded = {r["jid"]: r["reason"] for r in report["excluded"]}
    assert admitted == {SFC_ONLY}
    assert excluded[SAVED] == "in the address book"
    assert excluded[BOTH].startswith("also in 1 group outside")
    assert excluded[COMMUNITY_ONLY] == "on denyRead"
    assert OUTSIDER not in excluded and ME not in excluded
    assert report["admitted"][0]["name"] == "Person 0"
    # Readable through allowRead whatever the rule says.
    assert {r["jid"] for r in report["explicit"]} == {FRIEND}
    assert FRIEND not in excluded
