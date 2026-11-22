"""Which DMs the memberDMs rule admits for an instance, and why others are
kept out.

    WHATSAPP_MCP_ALLOWLIST=~/.config/whatsapp-mcp/allowlists/sfc.allowlist.json \\
        uv run python access_report.py

Covers people who are in one of the instance's readable groups and have a DM
chat with the account; prints names, so it is for the account owner, not a
tool for MCP clients.
"""
from typing import Any, Dict, List

import access_control
from db import connect, resolve_sender_names


def member_dm_report() -> Dict[str, List[Dict[str, Any]]]:
    config = access_control._current()
    conn = connect()
    try:
        cursor = conn.cursor()
        readable = access_control._expand(cursor, config.read)
        candidates = access_control.member_dm_candidates(cursor, readable)
        cursor.execute(
            "SELECT jid FROM chats WHERE jid LIKE '%@s.whatsapp.net' OR jid LIKE '%@lid'"
        )
        with_dm = {row[0] for row in cursor.fetchall()}
        people = sorted(jid for jid in candidates if jid in with_dm)
        saved = access_control.saved_contacts(cursor, people)
        denied = access_control._expand_aliases(cursor, config.deny)
        names = resolve_sender_names(cursor, people)
    finally:
        conn.close()

    admitted, excluded, explicit = [], [], []
    for jid in people:
        entry = {"jid": jid, "name": names.get(jid, jid)}
        outside = candidates[jid]
        if jid in denied:
            excluded.append({**entry, "reason": "on denyRead"})
        elif jid in readable:
            explicit.append(entry)
        elif jid in saved:
            excluded.append({**entry, "reason": "in the address book"})
        elif outside:
            n = len(outside)
            excluded.append({**entry, "reason": f"also in {n} group{'' if n == 1 else 's'} outside this instance's scope"})
        else:
            admitted.append(entry)
    return {"admitted": admitted, "excluded": excluded, "explicit": explicit}


def main() -> None:
    config = access_control._current()
    report = member_dm_report()
    state = "on" if config.member_dms else "OFF (set \"memberDMs\": true to apply)"
    print(f"Instance '{config.name}': memberDMs {state}\n")
    print(f"Admitted ({len(report['admitted'])}):")
    for r in report["admitted"]:
        print(f"  {r['name']}  {r['jid']}")
    print(f"\nAlready readable through allowRead ({len(report['explicit'])}):")
    for r in report["explicit"]:
        print(f"  {r['name']}  {r['jid']}")
    print(f"\nKept out ({len(report['excluded'])}):")
    for r in sorted(report["excluded"], key=lambda r: (r["reason"], r["name"])):
        print(f"  {r['name']}  {r['jid']}  – {r['reason']}")


if __name__ == "__main__":
    main()
