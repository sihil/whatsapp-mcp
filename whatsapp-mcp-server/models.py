"""Dataclasses for the rows the message store holds, plus the `to_dict()`
serializations the MCP tools return."""
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Dict, List, Optional


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None
    sender_name: Optional[str] = None
    edited: bool = False
    deleted: bool = False
    reactions: List[Dict[str, Any]] = field(default_factory=list)

    def to_dict(self) -> Dict[str, Any]:
        """`sender_name` is "Me" for own messages, the contact/chat name when
        known, and falls back to the raw sender JID otherwise. A deleted message
        keeps its row but has empty `content`; `reactions` holds one
        {emoji, sender, sender_name, is_from_me} per reactor."""
        return {
            "id": self.id,
            "chat_jid": self.chat_jid,
            "chat_name": self.chat_name,
            "sender": self.sender,
            "sender_name": self.sender_name,
            "timestamp": self.timestamp.isoformat() if self.timestamp else None,
            "content": self.content,
            "is_from_me": bool(self.is_from_me),
            "media_type": self.media_type,
            "edited": bool(self.edited),
            "deleted": bool(self.deleted),
            "reactions": self.reactions,
        }


@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

    def to_dict(self) -> Dict[str, Any]:
        return {
            "jid": self.jid,
            "name": self.name,
            "is_group": self.is_group,
            "last_message_time": self.last_message_time.isoformat() if self.last_message_time else None,
            "last_message": self.last_message,
            "last_sender": self.last_sender,
            "last_is_from_me": None if self.last_is_from_me is None else bool(self.last_is_from_me),
        }


@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str

    def to_dict(self) -> Dict[str, Any]:
        return {
            "phone_number": self.phone_number,
            "name": self.name,
            "jid": self.jid,
        }


@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]

    def to_dict(self) -> Dict[str, Any]:
        return {
            "message": self.message.to_dict(),
            "before": [message.to_dict() for message in self.before],
            "after": [message.to_dict() for message in self.after],
        }
