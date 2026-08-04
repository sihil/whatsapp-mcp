"""Runtime configuration shared by every module of the WhatsApp MCP server."""
import os

_DEFAULT_DB_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db'
)
_DEFAULT_API_BASE_URL = "http://localhost:8080/api"


def messages_db_path() -> str:
    """Path to the bridge's message store. `WHATSAPP_MCP_DB_PATH` overrides the
    default location relative to this file."""
    return os.environ.get("WHATSAPP_MCP_DB_PATH") or _DEFAULT_DB_PATH


def api_base_url() -> str:
    """Base URL of the bridge's REST API, overridable via `WHATSAPP_API_BASE_URL`."""
    return (os.environ.get("WHATSAPP_API_BASE_URL") or _DEFAULT_API_BASE_URL).rstrip("/")
