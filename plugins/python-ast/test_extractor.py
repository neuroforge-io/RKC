import ast
import hashlib
import importlib.util
import io
import json
import pathlib
import sys
import tempfile
import unittest
from unittest import mock

MODULE_PATH = pathlib.Path(__file__).with_name("extractor.py")
SPEC = importlib.util.spec_from_file_location("rkc_python_ast", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


class ExtractorTests(unittest.TestCase):
    def file_ref(self, root, path, source, language="python"):
        target = root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(source)
        return {
            "id": "rkc:artifact:test",
            "path": path,
            "language": language,
            "sha256": hashlib.sha256(source).hexdigest(),
            "size_bytes": len(source),
        }

    def test_signature_preserves_arguments(self):
        tree = ast.parse("def f(a: int, b='x', *args, c: bool = False, **kwargs) -> str:\n    return str(a)\n")
        node = tree.body[0]
        signature, arguments = MODULE.signature_for_function(node)
        self.assertIn("a: int", signature)
        self.assertIn("b = 'x'", signature)
        self.assertEqual([item["name"] for item in arguments], ["a", "b", "args", "c", "kwargs"])
        self.assertTrue(arguments[0]["required"])
        self.assertFalse(arguments[1]["required"])

    def test_module_name(self):
        self.assertEqual(MODULE.module_name("pkg/auth/service.py"), "pkg.auth.service")
        self.assertEqual(MODULE.module_name("pkg/__init__.py"), "pkg")

    def test_verified_source_enforces_path_size_digest_and_type(self):
        source = b"def example():\n    return True\n"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            reference = self.file_ref(root, "pkg/example.py", source)
            self.assertEqual(MODULE.read_verified_source(root, reference), source)

            version_one_reference = dict(reference)
            del version_one_reference["size_bytes"]
            self.assertEqual(
                MODULE.read_verified_source(root, version_one_reference), source
            )

            invalid = dict(reference)
            invalid["sha256"] = "0" * 64
            with self.assertRaisesRegex(ValueError, "content does not match inventory"):
                MODULE.read_verified_source(root, invalid)

            invalid = dict(reference)
            invalid["size_bytes"] = len(source) + 1
            with self.assertRaisesRegex(ValueError, "size does not match inventory"):
                MODULE.read_verified_source(root, invalid)

            for invalid_size in (-1, sys.maxsize + 1, True):
                invalid = dict(reference)
                invalid["size_bytes"] = invalid_size
                with self.assertRaisesRegex(ValueError, "invalid size"):
                    MODULE.read_verified_source(root, invalid)

            invalid = dict(reference)
            invalid["path"] = "..\\outside.py"
            with self.assertRaisesRegex(ValueError, "canonical slash-separated"):
                MODULE.read_verified_source(root, invalid)

            directory_ref = dict(reference)
            directory_ref["path"] = "pkg"
            with self.assertRaisesRegex(ValueError, "not a regular file"):
                MODULE.read_verified_source(root, directory_ref)

    def test_verified_source_rejects_symlinks_and_changed_content(self):
        source = b"def original():\n    return True\n"
        replacement = b"def replaced():\n    return False\n"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            reference = self.file_ref(root, "source.py", source)
            (root / "source.py").write_bytes(replacement)
            with self.assertRaisesRegex(ValueError, "size does not match inventory|content does not match inventory"):
                MODULE.read_verified_source(root, reference)

            target_ref = self.file_ref(root, "target.py", source)
            link = root / "link.py"
            try:
                link.symlink_to(root / "target.py")
            except OSError as exc:
                self.skipTest(f"symlinks unavailable: {exc}")
            link_ref = dict(target_ref)
            link_ref["path"] = "link.py"
            with self.assertRaisesRegex(ValueError, "contains a symlink"):
                MODULE.read_verified_source(root, link_ref)

    def test_main_verifies_every_file_before_emitting_output(self):
        source = b"def valid():\n    return True\n"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            valid = self.file_ref(root, "a.py", source)
            changed = self.file_ref(root, "z.txt", source, language="text")
            changed["sha256"] = "0" * 64
            request = json.dumps({"root": str(root), "files": [valid, changed]})
            output = io.StringIO()
            with mock.patch.object(MODULE.sys, "stdin", io.StringIO(request)), mock.patch.object(
                MODULE.sys, "stdout", output
            ):
                with self.assertRaisesRegex(ValueError, "content does not match inventory"):
                    MODULE.main()
            self.assertEqual(output.getvalue(), "")

    def test_invalid_utf8_remains_a_grounded_parse_diagnostic(self):
        source = b"\xff\xfe"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            reference = self.file_ref(root, "invalid.py", source)
            fragment = MODULE.process_file(
                reference, MODULE.read_verified_source(root, reference)
            )
            self.assertEqual(fragment["nodes"], [])
            self.assertEqual(fragment["edges"], [])
            self.assertEqual(fragment["evidence"], [])
            self.assertEqual(len(fragment["diagnostics"]), 1)
            self.assertEqual(fragment["diagnostics"][0]["code"], "RKC-PY-1001")
            self.assertEqual(
                fragment["diagnostics"][0]["source"]["artifact_id"], reference["id"]
            )


if __name__ == "__main__":
    unittest.main()
