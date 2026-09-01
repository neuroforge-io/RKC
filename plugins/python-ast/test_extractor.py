import ast
import hashlib
import importlib.util
import io
import json
import pathlib
import sys
import stat
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

MODULE_PATH = pathlib.Path(__file__).with_name("extractor.py")
SPEC = importlib.util.spec_from_file_location("rkc_python_ast", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)

INTERNAL_MODULE_PATH = pathlib.Path(__file__).parents[2] / "internal" / "builtinplugins" / "python_extractor.py"
INTERNAL_SPEC = importlib.util.spec_from_file_location(
    "rkc_internal_python_extractor", INTERNAL_MODULE_PATH
)
INTERNAL_MODULE = importlib.util.module_from_spec(INTERNAL_SPEC)
sys.modules[INTERNAL_SPEC.name] = INTERNAL_MODULE
assert INTERNAL_SPEC.loader is not None
INTERNAL_SPEC.loader.exec_module(INTERNAL_MODULE)


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

    def test_helper_boundaries_and_rich_ast_shapes(self):
        source = """\
import os as operating, sys
from .helpers import useful as alias
class _Private(BaseThing):
    @decorator
    async def method(self, value: int = 1, /, *args, flag: bool = False, **kwargs) -> str:
        return operating.path.join(value)
async def test_async(value):
    return (lambda: value)()
def _hidden():
    return alias(useful())
top_level()
"""
        for module in (MODULE, INTERNAL_MODULE):
            tree = ast.parse(source)
            class_node = tree.body[2]
            async_node = class_node.body[0]
            signature, arguments = module.signature_for_function(async_node)
            self.assertTrue(signature.startswith("async def method("))
            self.assertIn("*args", signature)
            self.assertIn("**kwargs", signature)
            self.assertEqual(arguments[0]["kind"], "positional_only")
            self.assertEqual(arguments[-1]["kind"], "variadic_keyword")
            self.assertEqual(module.visibility("_Private"), "private")
            self.assertEqual(module.visibility("__dunder__"), "special")
            self.assertEqual(module.visibility("public"), "public")
            self.assertEqual(module.callee_name(ast.parse("f[0]()").body[0].value), "f[0]")
            self.assertEqual(module.callee_name(ast.parse("factory().method()").body[0].value), "method")
            with mock.patch.object(module.ast, "unparse", side_effect=RuntimeError("broken")):
                self.assertEqual(module.unparse(ast.Name(id="x")), "Name")
            self.assertEqual(module.canonical_file_path("pkg/file.py"), "pkg/file.py")
            self.assertEqual(module.canonical_file_digest("a" * 64), "a" * 64)
            self.assertEqual(module.module_name("README"), "README")
            with mock.patch.object(module.hashlib, "sha256", return_value=SimpleNamespace(digest_size=31)):
                with self.assertRaisesRegex(ValueError, "SHA-256"):
                    module.canonical_file_digest("a" * 64)
            with tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory).resolve()
                reference = self.file_ref(root, "pkg/rich.py", source.encode())
                fragment = module.process_file(reference, source.encode())
                kinds = {item["kind"] for item in fragment["nodes"]}
                self.assertTrue({"module", "class", "method", "test", "function"} <= kinds)
                self.assertTrue(any(item["kind"] == "calls" for item in fragment["edges"]))
                context = module.Context(
                    "pkg/rich.py",
                    reference["id"],
                    module.node_id("module", "pkg/rich.py", "pkg.rich"),
                    "pkg.rich",
                    [],
                    [],
                    [],
                    [],
                )
                first = module.make_placeholder(context, "unresolved_symbol", "same", "call")
                self.assertEqual(
                    module.make_placeholder(context, "unresolved_symbol", "same", "call"),
                    first,
                )

    def test_canonical_helpers_reject_all_noncanonical_inputs(self):
        for module in (MODULE, INTERNAL_MODULE):
            for value in (None, "", "a\\b", "a\x00b", "/absolute.py", ".", "a/./b.py", "a/../b.py", "a//b.py"):
                with self.assertRaisesRegex(ValueError, "canonical slash-separated"):
                    module.canonical_file_path(value)
            for value in (None, "", "A" * 64, "g" * 64, "a" * 63, "a" * 65, True):
                with self.assertRaisesRegex(ValueError, "SHA-256"):
                    module.canonical_file_digest(value)

    def test_module_name(self):
        for module in (MODULE, INTERNAL_MODULE):
            self.assertEqual(module.module_name("pkg/auth/service.py"), "pkg.auth.service")
            self.assertEqual(module.module_name("pkg/__init__.py"), "pkg")

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

    def test_verified_source_rejects_remaining_identity_and_size_boundaries(self):
        source = b"value = 1\n"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            reference = self.file_ref(root, "source.py", source)
            with self.assertRaisesRegex(ValueError, "reference must be an object"):
                MODULE.read_verified_source(root, None)
            package_ref = dict(reference)
            package_ref["path"] = "pkg/file.py"
            (root / "pkg").write_bytes(b"not a directory")
            with self.assertRaisesRegex(ValueError, "non-directory component"):
                MODULE.read_verified_source(root, package_ref)
            with mock.patch.object(pathlib.Path, "resolve", side_effect=ValueError("escape")):
                with self.assertRaisesRegex(ValueError, "escapes the request root"):
                    MODULE.read_verified_source(root, reference)

            info = (root / "source.py").lstat()
            altered = mock.Mock(
                st_mode=info.st_mode,
                st_ino=info.st_ino + 1,
                st_dev=info.st_dev,
            )
            changed = pathlib.Path(directory) / "changed.py"
            changed.write_bytes(source)
            changed_ref = self.file_ref(root, "changed.py", source)
            with mock.patch.object(MODULE.os, "fstat", side_effect=[altered, info]):
                with self.assertRaisesRegex(ValueError, "identity changed while opening"):
                    MODULE.read_verified_source(root, reference)
            with mock.patch.object(MODULE.os, "fstat", side_effect=[info, info, info]):
                with mock.patch.object(pathlib.Path, "open", autospec=True) as open_mock:
                    stream = mock.MagicMock()
                    stream.__enter__.return_value = stream
                    stream.fileno.return_value = 1
                    stream.read.side_effect = [source[:3], b"", b""]
                    open_mock.return_value = stream
                    with self.assertRaisesRegex(ValueError, "became shorter"):
                        MODULE.read_verified_source(root, reference)
            with mock.patch.object(MODULE.os, "fstat", side_effect=[info, info, info]):
                with mock.patch.object(pathlib.Path, "open", autospec=True) as open_mock:
                    stream = mock.MagicMock()
                    stream.__enter__.return_value = stream
                    stream.fileno.return_value = 1
                    stream.read.side_effect = [source, b"extra"]
                    open_mock.return_value = stream
                    with self.assertRaisesRegex(ValueError, "grew beyond inventory"):
                        MODULE.read_verified_source(root, reference)
            altered_final = mock.Mock(
                st_mode=info.st_mode,
                st_ino=info.st_ino,
                st_dev=info.st_dev,
                st_size=info.st_size + 1,
                st_mtime_ns=info.st_mtime_ns,
            )
            with mock.patch.object(MODULE.os, "fstat", side_effect=[info, altered_final]):
                with self.assertRaisesRegex(ValueError, "identity changed while reading"):
                    MODULE.read_verified_source(root, reference)
            altered_path = mock.Mock(
                st_mode=info.st_mode,
                st_ino=info.st_ino + 1,
                st_dev=info.st_dev,
            )
            with mock.patch.object(pathlib.Path, "lstat", side_effect=[info, altered_path]):
                with self.assertRaisesRegex(ValueError, "path identity changed while reading"):
                    MODULE.read_verified_source(root, reference)
            self.assertEqual(changed_ref["size_bytes"], len(source))

    def test_internal_copy_covers_verified_source_and_main_success(self):
        source = b"def valid():\n    return True\n"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            python_ref = self.file_ref(root, "pkg/valid.py", source)
            text_ref = self.file_ref(root, "README.txt", b"documentation", language="text")
            for module in (MODULE, INTERNAL_MODULE):
                self.assertEqual(module.read_verified_source(root, python_ref), source)
                without_size = dict(python_ref)
                without_size.pop("size_bytes")
                self.assertEqual(module.read_verified_source(root, without_size), source)
                bad_digest = dict(python_ref, sha256="0" * 64)
                with self.assertRaisesRegex(ValueError, "content does not match inventory"):
                    module.read_verified_source(root, bad_digest)
                request = json.dumps({"root": str(root), "files": [text_ref, python_ref]})
                output = io.StringIO()
                with mock.patch.object(module.sys, "stdin", io.StringIO(request)), mock.patch.object(
                    module.sys, "stdout", output
                ):
                    self.assertEqual(module.main(), 0)
                payload = json.loads(output.getvalue())
                self.assertTrue(any(item["kind"] == "function" for item in payload["nodes"]))

    def test_verified_source_error_contract_is_identical_for_both_entrypoints(self):
        source = b"value = 1\n"
        for module in (MODULE, INTERNAL_MODULE):
            with self.subTest(module=module.__name__), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory).resolve()
                reference = self.file_ref(root, "source.py", source)
                with self.assertRaisesRegex(ValueError, "reference must be an object"):
                    module.read_verified_source(root, None)
                for bad_size in (-1, True):
                    bad_ref = dict(reference, size_bytes=bad_size)
                    with self.assertRaisesRegex(ValueError, "invalid size"):
                        module.read_verified_source(root, bad_ref)
                bad_size_ref = dict(reference, size_bytes=len(source) + 1)
                with self.assertRaisesRegex(ValueError, "size does not match inventory"):
                    module.read_verified_source(root, bad_size_ref)
                directory_ref = dict(reference, path="directory")
                (root / "directory").mkdir()
                with self.assertRaisesRegex(ValueError, "not a regular file"):
                    module.read_verified_source(root, directory_ref)
                package_ref = dict(reference, path="pkg/file.py")
                (root / "pkg").write_bytes(b"not a directory")
                with self.assertRaisesRegex(ValueError, "non-directory component"):
                    module.read_verified_source(root, package_ref)
                target_ref = self.file_ref(root, "target.py", source)
                link = root / "link.py"
                try:
                    link.symlink_to(root / "target.py")
                except OSError as exc:
                    self.skipTest(f"symlinks unavailable: {exc}")
                with self.assertRaisesRegex(ValueError, "contains a symlink"):
                    module.read_verified_source(root, dict(target_ref, path="link.py"))
                with mock.patch.object(pathlib.Path, "resolve", side_effect=ValueError("escape")):
                    with self.assertRaisesRegex(ValueError, "escapes the request root"):
                        module.read_verified_source(root, reference)
                info = (root / "source.py").lstat()
                nonregular = SimpleNamespace(st_mode=stat.S_IFDIR)
                with mock.patch.object(module.os, "fstat", return_value=nonregular):
                    with self.assertRaisesRegex(ValueError, "identity changed while opening"):
                        module.read_verified_source(root, reference)
                for read_result, message in (([source[:1], b""], "became shorter"), ([source, b"extra"], "grew beyond inventory")):
                    with mock.patch.object(module.os, "fstat", side_effect=[info, info, info]):
                        with mock.patch.object(pathlib.Path, "open", autospec=True) as open_mock:
                            stream = mock.MagicMock()
                            stream.__enter__.return_value = stream
                            stream.fileno.return_value = 1
                            stream.read.side_effect = read_result
                            open_mock.return_value = stream
                            with self.assertRaisesRegex(ValueError, message):
                                module.read_verified_source(root, reference)
                altered_final = SimpleNamespace(
                    st_mode=info.st_mode,
                    st_ino=info.st_ino,
                    st_dev=info.st_dev,
                    st_size=info.st_size + 1,
                    st_mtime_ns=info.st_mtime_ns,
                )
                with mock.patch.object(module.os, "fstat", side_effect=[info, altered_final]):
                    with self.assertRaisesRegex(ValueError, "identity changed while reading"):
                        module.read_verified_source(root, reference)
                altered_path = SimpleNamespace(
                    st_mode=info.st_mode,
                    st_ino=info.st_ino + 1,
                    st_dev=info.st_dev,
                )
                with mock.patch.object(pathlib.Path, "lstat", side_effect=[info, altered_path]):
                    with self.assertRaisesRegex(ValueError, "path identity changed while reading"):
                        module.read_verified_source(root, reference)

    def test_main_error_contract_and_final_root_race_for_both_entrypoints(self):
        for module in (MODULE, INTERNAL_MODULE):
            with self.subTest(module=module.__name__), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory).resolve()
                valid_request = {"root": str(root), "files": []}
                root_info = root.lstat()
                changed_root = SimpleNamespace(
                    st_mode=root_info.st_mode,
                    st_ino=root_info.st_ino + 1,
                    st_dev=root_info.st_dev,
                )
                with mock.patch.object(module.Path, "lstat", side_effect=[root_info, changed_root]):
                    with mock.patch.object(module.sys, "stdin", io.StringIO(json.dumps(valid_request))):
                        with self.assertRaisesRegex(ValueError, "identity changed while reading"):
                            module.main()
                symlink_info = SimpleNamespace(st_mode=stat.S_IFLNK)
                with mock.patch.object(module.Path, "lstat", return_value=symlink_info):
                    with mock.patch.object(module.sys, "stdin", io.StringIO(json.dumps(valid_request))):
                        with self.assertRaisesRegex(ValueError, "real directory"):
                            module.main()
                non_directory_info = SimpleNamespace(st_mode=stat.S_IFREG)
                with mock.patch.object(module.Path, "lstat", return_value=non_directory_info):
                    with mock.patch.object(module.sys, "stdin", io.StringIO(json.dumps(valid_request))):
                        with self.assertRaisesRegex(ValueError, "real directory"):
                            module.main()
                with mock.patch.object(module.os.path, "samestat", return_value=False):
                    with mock.patch.object(module.sys, "stdin", io.StringIO(json.dumps(valid_request))):
                        with self.assertRaisesRegex(ValueError, "identity changed while opening"):
                            module.main()

    def test_process_file_reports_syntax_errors(self):
        reference = {"id": "rkc:artifact:syntax", "path": "broken.py"}
        for module in (MODULE, INTERNAL_MODULE):
            fragment = module.process_file(reference, b"def broken(:\n")
            self.assertEqual(fragment["diagnostics"][0]["code"], "RKC-PY-1001")

    def test_main_guard_runs_for_each_extractor(self):
        request_source = b"value = 1\n"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            reference = self.file_ref(root, "value.py", request_source)
            request = json.dumps({"root": str(root), "files": [reference]})
            for module_path in (MODULE_PATH, INTERNAL_MODULE_PATH):
                with self.subTest(module_path=module_path):
                    output = io.StringIO()
                    with mock.patch.object(sys, "stdin", io.StringIO(request)), mock.patch.object(
                        sys, "stdout", output
                    ), mock.patch.object(sys, "argv", [str(module_path)]):
                        with self.assertRaises(SystemExit) as raised:
                            import runpy

                            runpy.run_path(str(module_path), run_name="__main__")
                    self.assertEqual(raised.exception.code, 0)
                    self.assertIn('"nodes"', output.getvalue())

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

    def test_main_rejects_invalid_request_shapes_and_root_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            cases = [
                ({}, "root must be a nonempty path"),
                ({"root": 1}, "root must be a nonempty path"),
                ({"root": str(root), "files": {}}, "files must be a list"),
                ({"root": str(root), "files": [None]}, "file reference must be an object"),
            ]
            for request, message in cases:
                with self.subTest(request=request):
                    with mock.patch.object(MODULE.sys, "stdin", io.StringIO(json.dumps(request))):
                        with self.assertRaisesRegex(ValueError, message):
                            MODULE.main()
            file_path = root / "file.py"
            file_path.write_bytes(b"x = 1\n")
            ref = self.file_ref(root, "file.py", b"x = 1\n")
            with mock.patch.object(MODULE.Path, "lstat", return_value=mock.Mock(st_mode=stat.S_IFREG)):
                with self.assertRaisesRegex(ValueError, "real directory"):
                    with mock.patch.object(MODULE.sys, "stdin", io.StringIO(json.dumps({"root": str(root), "files": []}))):
                        MODULE.main()
            real_root_info = root.lstat()
            changed_info = mock.Mock(st_mode=stat.S_IFDIR, st_ino=real_root_info.st_ino + 1, st_dev=real_root_info.st_dev)
            with mock.patch.object(MODULE.os.path, "samestat", return_value=False):
                with self.assertRaisesRegex(ValueError, "identity changed while opening"):
                    with mock.patch.object(MODULE.sys, "stdin", io.StringIO(json.dumps({"root": str(root), "files": [ref]}))):
                        MODULE.main()
            for request, message in (
                ({}, "root must be a nonempty path"),
                ({"root": str(root), "files": {}}, "files must be a list"),
                ({"root": str(root), "files": [None]}, "file reference must be an object"),
            ):
                with self.subTest(module=INTERNAL_MODULE.__name__, request=request):
                    with mock.patch.object(
                        INTERNAL_MODULE.sys, "stdin", io.StringIO(json.dumps(request))
                    ):
                        with self.assertRaisesRegex(ValueError, message):
                            INTERNAL_MODULE.main()

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
