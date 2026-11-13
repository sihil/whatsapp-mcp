"""Diagnostics about the connected MCP client.

`describe_client` exists to find out whether the client (Claude Desktop in
particular) declares MCP roots and which directories they name - the
candidate places for download_media to save files a session can open.
"""
import logging
import warnings
from typing import Any, Dict

from mcp.server.mcpserver import Context
from mcp.types import ToolAnnotations

from server import mcp

logger = logging.getLogger(__name__)


async def describe_session(session: Any) -> Dict[str, Any]:
    capabilities = session.client_capabilities
    params = session.client_params
    result: Dict[str, Any] = {
        "protocol_version": session.protocol_version,
        "client": params.client_info.model_dump(exclude_none=True)
        if params and params.client_info else None,
        "capabilities": capabilities.model_dump(exclude_none=True) if capabilities else None,
        "roots": None,
        "roots_error": None,
    }
    if capabilities is None or capabilities.roots is None:
        result["roots_error"] = "the client did not declare the roots capability"
    else:
        try:
            with warnings.catch_warnings():
                # Roots are deprecated from protocol 2026-07-28 (SEP-2577), but
                # older clients may still answer.
                warnings.simplefilter("ignore")
                listed = await session.list_roots()
            result["roots"] = [{"uri": str(r.uri), "name": r.name} for r in listed.roots]
        except Exception as e:  # any failure is the answer being sought
            result["roots_error"] = f"{type(e).__name__}: {e}"
    logger.info("MCP client description: %s", result)
    return result


@mcp.tool(
    title="Describe MCP Client",
    annotations=ToolAnnotations(readOnlyHint=True, openWorldHint=False),
)
async def describe_client(ctx: Context) -> Dict[str, Any]:
    """Diagnostic: report the connected MCP client's name, protocol version,
    declared capabilities and, if it supports them, its roots (the directories
    it exposes to MCP servers). Reads nothing from WhatsApp."""
    return await describe_session(ctx.session)
