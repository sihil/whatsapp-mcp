"""MCPServer instance shared by the tool modules."""
import config  # noqa: F401  configures logging as an import side effect
from mcp.server.mcpserver import MCPServer

mcp = MCPServer("whatsapp")
