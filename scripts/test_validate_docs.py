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
FOOTER = """---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
"""


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
        self.write("docs/target.md", "# Target\n\n" + FOOTER)
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
\n""" + FOOTER,
        )
        self.write("dist/broken.md", "[ignored](missing)\n```\n")
        self.write(".rkc-generated/broken.md", "[ignored](missing)\n```\n")
        self.write("LICENSES/vendor/LICENSE.md", "third-party terms\n")
        self.write("third_party/vendor/NOTICE.md", "third-party notice\n")
        self.write("vendor/upstream/README.md", "vendored documentation\n")
        self.write("THIRD_PARTY_NOTICES.md", "governed notice inventory\n")

        status, report = self.run_validator()

        self.assertEqual(status, 0)
        self.assertTrue(report["ok"])
        self.assertEqual(report["files_checked"], 6)
        self.assertEqual(report["issues"], [])

    def test_reports_missing_or_nonterminal_first_party_footer(self) -> None:
        self.write("README.md", "# Missing footer\n")
        self.write("docs/not-a-footer.md", FOOTER + "\n# Content after footer\n")

        status, report = self.run_validator()

        self.assertEqual(status, 1)
        messages = [issue["message"] for issue in report["issues"]]
        self.assertEqual(
            messages.count("missing standard NeuroForgeIO/Apache-2.0 publisher and contributor footer"),
            2,
        )

    def test_reports_missing_and_escaping_links(self) -> None:
        self.write(
            "README.md",
            """# Invalid documentation

[missing](missing.md)
[escaping](../outside.md)
\n""" + FOOTER,
        )
        self.write(
            "docs/unclosed.md",
            """# Unclosed

```text
[ignored while fenced](also-missing.md)
```\n\n""" + FOOTER,
        )

        status, report = self.run_validator()

        self.assertEqual(status, 1)
        self.assertFalse(report["ok"])
        self.assertEqual(report["files_checked"], 2)
        issues = report["issues"]
        self.assertIsInstance(issues, list)
        messages = [issue["message"] for issue in issues]
        self.assertTrue(
            any("missing local link target" in message for message in messages)
        )
        self.assertTrue(
            any("local link escapes repository" in message for message in messages)
        )
        self.assertFalse(any("unclosed ``` code fence" in message for message in messages))

    def test_reports_unclosed_fence_alongside_footer_contract(self) -> None:
        self.write("README.md", "# Unclosed\n\n```text\n")

        status, report = self.run_validator()

        self.assertEqual(status, 1)
        messages = [issue["message"] for issue in report["issues"]]
        self.assertIn("unclosed ``` code fence", messages)
        self.assertIn(
            "missing standard NeuroForgeIO/Apache-2.0 publisher and contributor footer", messages
        )

    def test_query_only_link_is_local_noop_and_mixed_fence_stays_open(self) -> None:
        self.write(
            "README.md",
            """# Edge syntax

[query-only local reference](?view=compact)

```text
~~~ does not close a backtick fence
"""
            + FOOTER,
        )

        status, report = self.run_validator()

        self.assertEqual(status, 1)
        messages = [issue["message"] for issue in report["issues"]]
        self.assertEqual(messages, ["unclosed ``` code fence"])
        self.assertFalse(
            any("local link target" in message for message in messages), messages
        )


if __name__ == "__main__":
    unittest.main()
