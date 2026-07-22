from __future__ import annotations

import os
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("validate-dco.sh").resolve()
ZERO_SHA = "0" * 40
AUTHOR_NAME = "RKC Test Author"
AUTHOR_EMAIL = "rkc-author@example.invalid"
AUTHOR_IDENTITY = f"{AUTHOR_NAME} <{AUTHOR_EMAIL}>"


class ValidateDCOTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-dco-test-")
        self.repository = Path(self.temporary.name)
        self.git("init", "-q", "-b", "main")
        self.git("config", "commit.gpgsign", "false")
        self.git("config", "user.name", AUTHOR_NAME)
        self.git("config", "user.email", AUTHOR_EMAIL)
        self.import_root = self.commit("Import test repository\n", signed=False)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def git(
        self,
        *arguments: str,
        input_text: str | None = None,
        check: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["git", *arguments],
            cwd=self.repository,
            check=check,
            input=input_text,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

    def commit(
        self,
        subject: str,
        *,
        signed: bool,
        trailer_identity: str = AUTHOR_IDENTITY,
        body_after_trailer: str = "",
    ) -> str:
        message = subject.rstrip("\n") + "\n"
        if signed:
            message += f"\nSigned-off-by: {trailer_identity}\n"
        if body_after_trailer:
            message += f"\n{body_after_trailer.rstrip()}\n"
        self.git("commit", "--allow-empty", "-q", "-F", "-", input_text=message)
        return self.git("rev-parse", "HEAD").stdout.strip()

    def validate(self, base: str, head: str = "HEAD") -> subprocess.CompletedProcess[str]:
        environment = os.environ.copy()
        environment["RKC_DCO_IMPORT_ROOT"] = self.import_root
        return subprocess.run(
            ["sh", str(SCRIPT), base, head],
            cwd=self.repository,
            env=environment,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

    def test_zero_base_exempts_only_approved_import_root(self) -> None:
        self.commit("Signed contribution", signed=True)
        result = self.validate(ZERO_SHA)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("DCO validation: passed", result.stdout)

        self.git("checkout", "-q", "--orphan", "unrelated")
        unrelated = self.commit("Unrelated root", signed=True)
        result = self.validate(ZERO_SHA, unrelated)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not descended from the approved import root", result.stderr)
        result = self.validate(self.import_root, unrelated)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not descended from the approved import root", result.stderr)

    def test_zero_base_rejects_unsigned_post_import_commit(self) -> None:
        commit = self.commit("Unsigned contribution", signed=False)
        result = self.validate(ZERO_SHA, commit)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing author-matching Signed-off-by trailer", result.stderr)

    def test_requires_parsed_trailer_not_body_lookalike(self) -> None:
        message = (
            "Body lookalike\n\n"
            f"Signed-off-by: {AUTHOR_IDENTITY}\n\n"
            "This paragraph makes the preceding line part of the body.\n"
        )
        self.git("commit", "--allow-empty", "-q", "-F", "-", input_text=message)
        result = self.validate(self.import_root)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing author-matching Signed-off-by trailer", result.stderr)

    def test_requires_trailer_identity_to_match_commit_author(self) -> None:
        self.commit(
            "Mismatched sign-off",
            signed=True,
            trailer_identity="Different Person <different@example.invalid>",
        )
        result = self.validate(self.import_root)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(AUTHOR_IDENTITY, result.stderr)

    def test_accepts_author_matching_parsed_trailer(self) -> None:
        commit = self.commit("Signed contribution", signed=True)
        result = self.validate(self.import_root, commit)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
