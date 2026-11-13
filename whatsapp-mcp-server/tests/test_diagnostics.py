import asyncio
from types import SimpleNamespace

import mcp.types as types

from tools_diagnostics import describe_session


class FakeSession:
    def __init__(self, capabilities, roots=None, error=None):
        self.client_capabilities = capabilities
        self.client_params = types.InitializeRequestParams(
            protocol_version="2025-06-18",
            capabilities=capabilities or types.ClientCapabilities(),
            client_info=types.Implementation(name="claude-ai", version="0.1.0"),
        )
        self.protocol_version = "2025-06-18"
        self._roots, self._error = roots, error

    async def list_roots(self):
        if self._error:
            raise self._error
        return types.ListRootsResult(roots=self._roots)


def _describe(session):
    return asyncio.run(describe_session(session))


def test_reports_roots_when_client_declares_them():
    session = FakeSession(
        types.ClientCapabilities(roots=types.RootsCapability()),
        roots=[types.Root(uri="file:///Users/me/Documents/Claude", name="Claude")],
    )
    result = _describe(session)
    assert result["client"]["name"] == "claude-ai"
    assert result["protocol_version"] == "2025-06-18"
    assert result["roots"] == [{"uri": "file:///Users/me/Documents/Claude", "name": "Claude"}]
    assert result["roots_error"] is None
    assert "roots" in result["capabilities"]


def test_says_when_client_has_no_roots_capability():
    result = _describe(FakeSession(types.ClientCapabilities()))
    assert result["roots"] is None
    assert "did not declare" in result["roots_error"]


def test_reports_a_failed_roots_request():
    session = FakeSession(
        types.ClientCapabilities(roots=types.RootsCapability()),
        error=RuntimeError("no back-channel"),
    )
    result = _describe(session)
    assert result["roots"] is None
    assert "no back-channel" in result["roots_error"]


def test_tolerates_missing_client_params():
    session = SimpleNamespace(
        client_capabilities=None, client_params=None, protocol_version="2026-07-28",
    )
    result = _describe(session)
    assert result["client"] is None
    assert result["capabilities"] is None
