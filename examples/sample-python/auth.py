"""Small example used by the reference implementation tests."""

from dataclasses import dataclass
import hashlib


@dataclass
class Session:
    """Authenticated session token."""

    token: str
    user_id: str


class AuthenticationError(RuntimeError):
    """Raised when credentials are invalid."""


class AuthService:
    """Authenticates users against a supplied user store."""

    def __init__(self, users: dict[str, str]) -> None:
        self.users = users

    def login(self, username: str, password: str, remember: bool = False) -> Session:
        """Validate credentials and return a session."""
        expected = self.users.get(username)
        supplied = hashlib.sha256(password.encode("utf-8")).hexdigest()
        if expected != supplied:
            raise AuthenticationError(username)
        suffix = "persistent" if remember else "ephemeral"
        return Session(token=f"{username}:{suffix}", user_id=username)
