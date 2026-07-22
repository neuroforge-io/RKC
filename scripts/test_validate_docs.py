#!/usr/bin/env python3
"""Behavior tests for the dependency-free Markdown documentation validator."""
from __future__ import annotations

import io
import json
import runpy
import tempfile
import unittest
from pathlib import Path
from unittest import mock


VALIDATOR = Path(__file__).with_name("validate-docs.py").absolute()


class ValidateDocsTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-doc-validator-test-")
        self.root = Path(self.temporary.name)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write(self, relative: str, content: str) -> Path:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
        return path

    def run_validator(self) -> tuple[int, dict[str, object]]:
        """Execute the real script while redirecting only its derived ROOT."""
        original_resolve = Path.resolve
        synthetic_script = self.root / "scripts" / "validate-docs.py"

        def controlled_resolve(path: Path, *args: object, **kwargs: object) -> Path:
            if path.absolute() == VALIDATOR:
                return synthetic_script
            return original_resolve(path, *args, **kwargs)

        stdout = io.StringIO()
        status = 0
        with mock.patch.object(Path, "resolve", controlled_resolve), mock.patch(
            "sys.stdout", new=stdout
        ):
            try:
                runpy.run_path(str(VALIDATOR), run_name="rkc_validate_docs_test")
            except SystemExit as exc:
                status = int(exc.code or 0)
        return status, json.loads(stdout.getvalue())

    def test_accepts_canonical_links_fences_and_ignored_output_trees(self) -> None:
        self.write("docs/target.md", "# Target\n")
        self.write(
            "README.md",
            """# Valid documentation

[local](docs/target.md)
[angle](<docs/target.md>)
[query and title](docs/target.md?view=1#section "target")
[fragment](#valid-documentation)
[web](https://example.com/reference)
[mail](mailto:docs@example.com)
[phone](tel:+61000000000)
[sandbox](sandbox:/artifact)
[embedded](data:text/plain,ok)
![image links are outside this validator](missing.png)

```markdown
[missing links inside code are ignored](missing-in-code.md)
```

~~~text
[these are ignored too](another-missing-file)
~~~
""",
        )
        self.write("dist/broken.md", "[ignored](missing)\n```\n")
        self.write(".rkc-generated/broken.md", "[ignored](missing)\n```\n")

        status, report = self.run_validator()

        self.assertEqual(status, 0)
        self.assertTrue(report["ok"])
        self.assertEqual(report["files_checked"], 2)
        self.assertEqual(report["issues"], [])

    def test_reports_missing_escaping_and_unclosed_fence(self) -> None:
        self.write(
            "README.md",
            """# Invalid documentation

[missing](missing.md)
[escaping](../outside.md)
""",
        )
        self.write(
            "docs/unclosed.md",
            """# Unclosed

```text
[ignored while fenced](also-missing.md)
""",
        )

        status, report = self.run_validator()

        self.assertEqual(status, 1)
        self.assertFalse(report["ok"])
        self.assertEqual(report["files_checked"], 2)
        issues = report["issues"]
        self.assertIsInstance(issues, list)
        messages = [issue["message"] for issue in issues]
        self.assertTrue(any("missing local link target" in message for message in messages))
        self.assertTrue(any("local link escapes repository" in message for message in messages))
        self.assertTrue(any("unclosed ``` code fence" in message for message in messages))


if __name__ == "__main__":
    unittest.main()
