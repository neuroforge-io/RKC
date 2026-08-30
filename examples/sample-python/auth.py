"""Small example used by the reference implementation tests."""

from dataclasses import dataclass
import hashlib
import hmac
import secrets


PASSWORD_ALGORITHM = "pbkdf2_sha256"
PASSWORD_DIGEST_BYTES = 32
PASSWORD_ITERATIONS = 600_000
PASSWORD_SALT_BYTES = 16


@dataclass
class Session:
    """Authenticated session token."""

    token: str
    user_id: str
    persistent: bool


class AuthenticationError(RuntimeError):
    """Raised when credentials are invalid."""


def encode_password(password: str, *, salt: bytes) -> str:
    """Return a salted, deliberately expensive password verifier."""
    if len(salt) < PASSWORD_SALT_BYTES:
        raise ValueError("password salt must contain at least 16 bytes")
    digest = hashlib.pbkdf2_hmac(
        "sha256", password.encode("utf-8"), salt, PASSWORD_ITERATIONS
    )
    return "$".join(
        (PASSWORD_ALGORITHM, str(PASSWORD_ITERATIONS), salt.hex(), digest.hex())
    )


def verify_password(password: str, encoded: str) -> bool:
    """Verify one encoded password without leaking digest comparisons."""
    try:
        algorithm, iterations_text, salt_text, expected_text = encoded.split("$")
        iterations = int(iterations_text)
        salt = bytes.fromhex(salt_text)
        expected = bytes.fromhex(expected_text)
    except (TypeError, ValueError):
        return False
    if (
        algorithm != PASSWORD_ALGORITHM
        or iterations != PASSWORD_ITERATIONS
        or len(salt) < PASSWORD_SALT_BYTES
        or len(expected) != PASSWORD_DIGEST_BYTES
    ):
        return False
    supplied = hashlib.pbkdf2_hmac(
        "sha256", password.encode("utf-8"), salt, iterations
    )
    return hmac.compare_digest(supplied, expected)


class AuthService:
    """Authenticates users against a supplied user store."""

    def __init__(self, users: dict[str, str]) -> None:
        self.users = users

    def login(self, username: str, password: str, remember: bool = False) -> Session:
        """Validate credentials and return a session."""
        expected = self.users.get(username)
        if expected is None or not verify_password(password, expected):
            raise AuthenticationError(username)
        return Session(
            token=secrets.token_urlsafe(32), user_id=username, persistent=remember
        )
