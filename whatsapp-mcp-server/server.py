"""FastMCP server instance shared by the tool modules."""
import config  # noqa: F401  configures logging as an import side effect
from mcp.server.fastmcp import FastMCP

mcp = FastMCP("whatsapp")
