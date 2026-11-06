import asyncio

import pytest
from mcp.server.mcpserver.exceptions import ToolError, UnexpectedToolError

from conftest import OTHER_GROUP
from main import mcp


def _call(name, arguments):
    return asyncio.run(mcp.call_tool(name, arguments))


def test_refusal_reaches_the_model_with_its_reason(store):
    with pytest.raises(ToolError, match="read allowlist") as raised:
        _call("get_chat", {"chat_jid": OTHER_GROUP})
    assert not isinstance(raised.value, UnexpectedToolError)


def test_anticipated_failure_reaches_the_model_with_its_reason(store):
    with pytest.raises(ToolError, match="Message with ID nope not found") as raised:
        _call("get_message_context", {"message_id": "nope"})
    assert not isinstance(raised.value, UnexpectedToolError)
