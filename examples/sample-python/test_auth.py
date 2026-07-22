import hashlib

from auth import AuthService


def test_login() -> None:
    users = {"lloyd": hashlib.sha256(b"correct horse").hexdigest()}
    session = AuthService(users).login("lloyd", "correct horse")
    assert session.user_id == "lloyd"
