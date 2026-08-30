import hashlib
import unittest

from auth import AuthService, AuthenticationError


class AuthTests(unittest.TestCase):
    def test_login(self) -> None:
        users = {"lloyd": hashlib.sha256(b"correct horse").hexdigest()}
        session = AuthService(users).login("lloyd", "correct horse", remember=True)
        self.assertEqual(session.user_id, "lloyd")
        self.assertEqual(session.token, "lloyd:persistent")


    def test_login_rejects_invalid_credentials(self) -> None:
        service = AuthService({"lloyd": hashlib.sha256(b"correct horse").hexdigest()})
        with self.assertRaises(AuthenticationError):
            service.login("lloyd", "wrong")


if __name__ == "__main__":
    unittest.main()
