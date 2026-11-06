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
    assert read == {"447700900003@s.whatsapp.net"}
    assert write == frozenset()
