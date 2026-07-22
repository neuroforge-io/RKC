import ast
import importlib.util
import pathlib
import sys
import unittest

MODULE_PATH = pathlib.Path(__file__).with_name("extractor.py")
SPEC = importlib.util.spec_from_file_location("rkc_python_ast", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


class ExtractorTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
