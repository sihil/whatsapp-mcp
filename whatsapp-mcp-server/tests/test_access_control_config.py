import json

import pytest

import access_control


def _load_with(monkeypatch, tmp_path, config):
    path = tmp_path / "list.allowlist.json"
    path.write_text(json.dumps(config))
    monkeypatch.setenv("WHATSAPP_MCP_ALLOWLIST", str(path))
    return access_control._load()


def test_missing_env_var_refuses_to_start(monkeypatch):
    monkeypatch.delenv("WHATSAPP_MCP_ALLOWLIST")
    with pytest.raises(SystemExit):
        access_control._load()


def test_missing_allow_read_refuses_to_start(monkeypatch, tmp_path):
    with pytest.raises(SystemExit):
        _load_with(monkeypatch, tmp_path, {"allowWrite": []})


def test_missing_allow_write_refuses_to_start(monkeypatch, tmp_path):
    with pytest.raises(SystemExit):
        _load_with(monkeypatch, tmp_path, {"allowRead": [{"id": "1@g.us"}]})


def test_empty_allow_write_means_read_only(monkeypatch, tmp_path):
    _, read, write = _load_with(
        monkeypatch, tmp_path, {"allowRead": [{"id": "447700900003"}], "allowWrite": []}
    )
    assert read.jids == {"447700900003@s.whatsapp.net"}
    assert write.jids == frozenset()


def test_community_entry_is_kept_apart(monkeypatch, tmp_path):
    _, read, _ = _load_with(
        monkeypatch, tmp_path,
        {"allowRead": [{"id": "1@g.us", "include": "community"}, {"id": "2@g.us"}], "allowWrite": []},
    )
    assert read.jids == {"2@g.us"}
    assert read.communities == {"1@g.us"}


def test_unknown_include_refuses_to_start(monkeypatch, tmp_path):
    with pytest.raises(SystemExit):
        _load_with(
            monkeypatch, tmp_path,
            {"allowRead": [{"id": "1@g.us", "include": "everything"}], "allowWrite": []},
        )
