from server import mcp
import tools_read  # noqa: F401  registers the read tools
import tools_send  # noqa: F401  registers the send/live-action tools
import tools_diagnostics  # noqa: F401  registers describe_client

if __name__ == "__main__":
    mcp.run(transport='stdio')
