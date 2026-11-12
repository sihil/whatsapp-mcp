"""Communities: which groups are linked to which community, as the bridge
records them in group_links."""
import sqlite3
from datetime import datetime
from typing import Any, Dict, List, Optional

import access_control
from api_client import get_community_members
from db import canonical_jid, connect


def _links_available(cursor: sqlite3.Cursor) -> bool:
    """Stores written before the bridge kept group_links know no communities."""
    cursor.execute("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'group_links'")
    return cursor.fetchone() is not None


def _community_jids(cursor: sqlite3.Cursor) -> List[str]:
    cursor.execute("""
        SELECT group_jid FROM group_links WHERE is_community = 1
        UNION SELECT parent_jid FROM group_links WHERE parent_jid <> ''
    """)
    return sorted(row[0] for row in cursor.fetchall())


def _name(cursor: sqlite3.Cursor, jid: str) -> Optional[str]:
    cursor.execute("""
        SELECT COALESCE(NULLIF(TRIM(l.name), ''), NULLIF(TRIM(c.name), ''))
        FROM (SELECT ? AS jid) j
        LEFT JOIN group_links l ON l.group_jid = j.jid
        LEFT JOIN chats c ON c.jid = j.jid
    """, (jid,))
    row = cursor.fetchone()
    return row[0] if row else None


def _describe(
    cursor: sqlite3.Cursor, community: str, readable: Optional[frozenset]
) -> Dict[str, Any]:
    cursor.execute("""
        SELECT l.group_jid, COALESCE(NULLIF(TRIM(l.name), ''), NULLIF(TRIM(c.name), '')),
               l.is_announcement, c.last_message_time
        FROM group_links l LEFT JOIN chats c ON c.jid = l.group_jid
        WHERE l.parent_jid = ?
        ORDER BY l.is_announcement DESC, 2, l.group_jid
    """, (community,))
    subgroups, hidden = [], 0
    for jid, name, is_announcement, last_message_time in cursor.fetchall():
        if readable is not None and jid not in readable:
            hidden += 1
            continue
        subgroups.append({
            "jid": jid,
            "name": name,
            "is_announcement": bool(is_announcement),
            "last_message_time": (
                datetime.fromisoformat(last_message_time).isoformat() if last_message_time else None
            ),
        })
    return {
        "jid": community,
        "name": _name(cursor, community),
        "readable": readable is None or community in readable,
        "subgroups": subgroups,
        "hidden_subgroups": hidden,
    }


def list_communities() -> List[Dict[str, Any]]:
    """Communities that are readable or have a readable group, each with its
    readable groups and a count of the groups this server may not read."""
    conn = connect()
    try:
        cursor = conn.cursor()
        if not _links_available(cursor):
            return []
        readable = access_control.readable_jids(cursor)
        found = [_describe(cursor, jid, readable) for jid in _community_jids(cursor)]
        return [c for c in found if c["readable"] or c["subgroups"]]
    finally:
        conn.close()


def get_community(community_jid: str, include_members: bool = False) -> Dict[str, Any]:
    """One readable community and its readable groups. With include_members,
    also everyone across its groups, which needs every group to be readable:
    the list would otherwise expose the members of groups this server may not
    read."""
    jid = canonical_jid(community_jid)
    conn = connect()
    try:
        cursor = conn.cursor()
        access_control.check_readable(jid, cursor)
        if not _links_available(cursor) or jid not in _community_jids(cursor):
            raise ValueError(f"{community_jid} is not a community the bridge knows of")
        community = _describe(cursor, jid, access_control.readable_jids(cursor))
    finally:
        conn.close()

    if include_members:
        if community["hidden_subgroups"]:
            raise PermissionError(
                f"Members of {community_jid} span {community['hidden_subgroups']} of its groups "
                f"outside the read allowlist of the '{access_control.INSTANCE_NAME}' WhatsApp "
                "MCP server; list members per group with list_group_members instead"
            )
        result = get_community_members(jid)
        if not result.get("success", False):
            raise RuntimeError(result.get("message", "Unknown error"))
        community["members"] = result.get("members", [])
        community["member_count"] = result.get("member_count", 0)
    return community
