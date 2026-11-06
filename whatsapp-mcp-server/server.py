"""MCPServer instance shared by the tool modules."""
import functools

import config  # noqa: F401  configures logging as an import side effect
from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError

mcp = MCPServer("whatsapp")


def tool(**kwargs):
    """`mcp.tool` that reports the failures the tools raise on purpose.

    The SDK passes only ToolError messages to the model; any other exception
    reaches it as a bare "Error executing tool <name>", hiding why a chat was
    refused or the bridge is unreachable.
    """
    def decorator(fn):
        @functools.wraps(fn)
        def wrapper(*args, **inner_kwargs):
            try:
                return fn(*args, **inner_kwargs)
            except (PermissionError, ValueError, RuntimeError) as e:
                raise ToolError(str(e)) from e

        return mcp.tool(**kwargs)(wrapper)

    return decorator
