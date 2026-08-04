"""Runtime configuration shared by every module of the WhatsApp MCP server."""
import os

MESSAGES_DB_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db'
)
WHATSAPP_API_BASE_URL = "http://localhost:8080/api"
