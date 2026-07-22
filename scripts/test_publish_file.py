from __future__ import annotations

import importlib.util
import io
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

MODULE_NAME = "rkc_publish_file"
SPEC = importlib.util.spec_from_file_location(
    MODULE_NAME, Path(__file__).with_name("publish_file.py")
)
assert SPEC and SPEC.loader
PUBLISHER = importlib.util.module_from_spec(SPEC)
sys.modules[MODULE_NAME] = PUBLISHER
SPEC.loader.exec_module(PUBLISHER)


class PublishFileTests(unittest.TestCase):
    def setUp(self) -> None:
        self.original_dist = PUBLISHER.DIST
        self.original_root = PUBLISHER.ROOT
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-publish-test-")
        root = Path(self.temporary.name)
        (root / "dist" / "logs").mkdir(parents=True)
        PUBLISHER.ROOT = root
        PUBLISHER.DIST = root / "dist"

    def tearDown(self) -> None:
        PUBLISHER.DIST = self.original_dist
        PUBLISHER.ROOT = self.original_root
        self.temporary.cleanup()

    def test_publish_replaces_only_regular_leaf(self) -> None:
        root = PUBLISHER.ROOT
        source = root / "source"
        source.write_bytes(b"new")
        destination = root / "dist" / "logs" / "result.txt"
        destination.write_bytes(b"old")
        PUBLISHER.publish(source, destination, 0o644)
        self.assertEqual(destination.read_bytes(), b"new")
        self.assertEqual(os.stat(destination).st_mode & 0o777, 0o644)

    def test_publish_rejects_link_leaf_and_link_parent(self) -> None:
        root = PUBLISHER.ROOT
        source = root / "source"
        source.write_bytes(b"new")
        outside = root / "outside"
        outside.mkdir()
        linked_leaf = root / "dist" / "logs" / "linked"
        linked_leaf.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(PUBLISHER.PublishError):
            PUBLISHER.publish(source, linked_leaf, 0o644)
        self.assertEqual(list(outside.iterdir()), [])

        linked_parent = root / "dist" / "linked-parent"
        linked_parent.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(PUBLISHER.PublishError):
            PUBLISHER.publish(source, linked_parent / "file", 0o644)
        self.assertEqual(list(outside.iterdir()), [])

    def test_publish_rejects_destination_outside_dist(self) -> None:
        root = PUBLISHER.ROOT
        source = root / "source"
        source.write_bytes(b"new")
        with self.assertRaises(PUBLISHER.PublishError):
            PUBLISHER.publish(source, root / "outside.txt", 0o644)

    def test_destination_identity_distinguishes_absent_regular_and_directory(self) -> None:
        directory = PUBLISHER.ROOT / "dist" / "logs"
        flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0)
        descriptor = os.open(directory, flags)
        try:
            self.assertIsNone(PUBLISHER.destination_identity(descriptor, "absent"))
            (directory / "regular").write_bytes(b"x")
            self.assertIsNotNone(PUBLISHER.destination_identity(descriptor, "regular"))
            (directory / "child").mkdir()
            with self.assertRaisesRegex(PUBLISHER.PublishError, "regular file"):
                PUBLISHER.destination_identity(descriptor, "child")
        finally:
            os.close(descriptor)

    def test_publish_rejects_bad_mode_source_type_and_same_inode(self) -> None:
        root = PUBLISHER.ROOT
        destination = root / "dist" / "logs" / "result"
        source = root / "source"
        source.write_bytes(b"content")
        with self.assertRaisesRegex(PUBLISHER.PublishError, "mode"):
            PUBLISHER.publish(source, destination, 0o600)
        with self.assertRaisesRegex(PUBLISHER.PublishError, "bounded regular"):
            PUBLISHER.publish(root, destination, 0o644)
        destination.write_bytes(b"content")
        source.unlink()
        os.link(destination, source)
        with self.assertRaisesRegex(PUBLISHER.PublishError, "same file"):
            PUBLISHER.publish(source, destination, 0o644)
        source.unlink()
        source.symlink_to(destination)
        with self.assertRaisesRegex(PUBLISHER.PublishError, "without following"):
            PUBLISHER.publish(source, destination, 0o644)

    def test_publish_requires_existing_real_parent(self) -> None:
        source = PUBLISHER.ROOT / "source"
        source.write_bytes(b"x")
        with self.assertRaisesRegex(PUBLISHER.PublishError, "parent"):
            PUBLISHER.publish(
                source,
                PUBLISHER.ROOT / "dist" / "missing" / "result",
                0o644,
            )

    def test_main_reports_failure_and_success(self) -> None:
        arguments = [
            "publish_file.py",
            "--source",
            "source",
            "--destination",
            "destination",
            "--mode",
            "0644",
        ]
        with mock.patch.object(sys, "argv", arguments), mock.patch.object(
            PUBLISHER, "publish"
        ) as publish:
            self.assertEqual(PUBLISHER.main(), 0)
            publish.assert_called_once()
        with mock.patch.object(sys, "argv", arguments), mock.patch.object(
            PUBLISHER, "publish", side_effect=PUBLISHER.PublishError("blocked")
        ), mock.patch("sys.stderr", new=io.StringIO()) as errors:
            self.assertEqual(PUBLISHER.main(), 1)
            self.assertIn("blocked", errors.getvalue())


if __name__ == "__main__":
    unittest.main()
