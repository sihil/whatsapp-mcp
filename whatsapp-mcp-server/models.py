"""Dataclasses for the rows the message store holds."""
from dataclasses import dataclass
from datetime import datetime
from typing import List, Optional


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


@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str


@dataclass
class GroupMember:
    jid: str
    phone_number: Optional[str]
    display_name: Optional[str]
    is_admin: bool
    is_super_admin: bool


@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]
