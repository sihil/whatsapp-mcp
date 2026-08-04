"""Runtime configuration shared by every module of the WhatsApp MCP server.

Importing this module configures logging exactly once. All logging goes to
stderr: stdout carries only JSON-RPC frames.
"""
import logging
import os
import sys
from typing import List, Optional

_DEFAULT_DB_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db'
)
_DEFAULT_API_BASE_URL = "http://localhost:8080/api"
_DEFAULT_SEND_RATE_LIMIT = 30

BRIDGE_DOWN_HINT = "is the WhatsApp bridge running? (cd whatsapp-bridge && go run .)"


def configure_logging() -> None:
    raw = os.environ.get("WHATSAPP_MCP_LOG_LEVEL")
    level = logging.getLevelName((raw or "INFO").upper())
    unknown = not isinstance(level, int)
    logging.basicConfig(
        level=logging.INFO if unknown else level,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        stream=sys.stderr,
    )
    if unknown:
        logging.getLogger(__name__).warning(
            f"Ignoring invalid WHATSAPP_MCP_LOG_LEVEL {raw!r}; using INFO."
        )


def messages_db_path() -> str:
    """Path to the bridge's message store. `WHATSAPP_MCP_DB_PATH` overrides the
    default location relative to this file."""
    return os.environ.get("WHATSAPP_MCP_DB_PATH") or _DEFAULT_DB_PATH


def api_base_url() -> str:
    """Base URL of the bridge's REST API, overridable via `WHATSAPP_API_BASE_URL`."""
    return (os.environ.get("WHATSAPP_API_BASE_URL") or _DEFAULT_API_BASE_URL).rstrip("/")


def allowed_media_dirs() -> Optional[List[str]]:
    """Allowlist of directories outbound media must live under, from
    `WHATSAPP_MCP_ALLOWED_MEDIA_DIRS`. `None` when unset means no restriction
    (current/default behavior).

    Entries are separated by `os.pathsep` — ':' on Unix, ';' on Windows.
    Splitting on ':' everywhere would cut Windows drive letters in half and
    reject every file under the directory the setting names.
    """
    raw = os.environ.get("WHATSAPP_MCP_ALLOWED_MEDIA_DIRS")
    if not raw:
        return None
    return [os.path.realpath(entry) for entry in raw.split(os.pathsep) if entry]


def send_rate_limit() -> int:
    """Max outbound sends per rolling minute, from `WHATSAPP_MCP_SEND_RATE_LIMIT`
    (default 30). `0` or negative disables the limiter."""
    raw = os.environ.get("WHATSAPP_MCP_SEND_RATE_LIMIT")
    if not raw:
        return _DEFAULT_SEND_RATE_LIMIT
    try:
        return int(raw)
    except ValueError:
        return _DEFAULT_SEND_RATE_LIMIT


configure_logging()
