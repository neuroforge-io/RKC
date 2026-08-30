import unittest

from auth import AuthService, AuthenticationError, encode_password, verify_password


TEST_SALT = b"rkc-auth-example"


class AuthTests(unittest.TestCase):
    def test_login(self) -> None:
        users = {"lloyd": encode_password("correct horse", salt=TEST_SALT)}
        session = AuthService(users).login("lloyd", "correct horse", remember=True)
        self.assertEqual(session.user_id, "lloyd")
        self.assertTrue(session.persistent)
        self.assertNotIn("lloyd", session.token)
        self.assertGreaterEqual(len(session.token), 40)


    def test_login_rejects_invalid_credentials(self) -> None:
        service = AuthService(
            {"lloyd": encode_password("correct horse", salt=TEST_SALT)}
        )
        with self.assertRaises(AuthenticationError):
            service.login("lloyd", "wrong")
        with self.assertRaises(AuthenticationError):
            AuthService({"lloyd": "malformed"}).login("lloyd", "correct horse")

    def test_password_verifier_rejects_malformed_parameters(self) -> None:
        with self.assertRaises(ValueError):
            encode_password("correct horse", salt=b"short")

        encoded = encode_password("correct horse", salt=TEST_SALT)
        algorithm, iterations, salt, digest = encoded.split("$")
        invalid = (
            f"other${iterations}${salt}${digest}",
            f"{algorithm}$1${salt}${digest}",
            f"{algorithm}${iterations}$00${digest}",
            f"{algorithm}${iterations}${salt}$00",
        )
        for candidate in invalid:
            self.assertFalse(verify_password("correct horse", candidate))


if __name__ == "__main__":
    unittest.main()
