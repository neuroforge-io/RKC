#!/usr/bin/env python3
"""Tests for atomic complete-generation publication."""
from __future__ import annotations

import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

MODULE_NAME = "rkc_publish_directory"
SPEC = importlib.util.spec_from_file_location(
    MODULE_NAME, Path(__file__).with_name("publish_directory.py")
)
assert SPEC and SPEC.loader
PUBLISH = importlib.util.module_from_spec(SPEC)
sys.modules[MODULE_NAME] = PUBLISH
SPEC.loader.exec_module(PUBLISH)


class PublishDirectoryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.original_root = PUBLISH.ROOT
        self.original_dist = PUBLISH.DIST
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-publish-directory-")
        PUBLISH.ROOT = Path(self.temporary.name)
        PUBLISH.DIST = PUBLISH.ROOT / "dist"
        PUBLISH.DIST.mkdir()

    def tearDown(self) -> None:
        PUBLISH.ROOT = self.original_root
        PUBLISH.DIST = self.original_dist
        self.temporary.cleanup()

    def stage(self, name: str, content: str) -> Path:
        source = PUBLISH.DIST / f".rkc-fixture.{name}" / name
        source.mkdir(parents=True)
        (source / "value.txt").write_text(content, encoding="utf-8")
        return source

    def test_staged_directories_are_synced_bottom_up(self) -> None:
        source = self.stage("nested", "value")
        nested = source / "one" / "two"
        nested.mkdir(parents=True)
        opened: list[Path] = []
        synced: list[int] = []

        def open_directory(path: Path, _flags: int) -> int:
            opened.append(Path(path))
            return len(opened) + 100

        with mock.patch.object(
            PUBLISH.os, "open", side_effect=open_directory
        ), mock.patch.object(
            PUBLISH.os,
            "fsync",
            side_effect=lambda descriptor: synced.append(descriptor),
        ), mock.patch.object(
            PUBLISH.os, "close"
        ):
            PUBLISH.sync_tree_directories(source)
        self.assertEqual(opened, [nested, nested.parent, source])
        self.assertEqual(synced, [101, 102, 103])

    def test_staged_directory_open_failure_is_wrapped(self) -> None:
        source = self.stage("unavailable", "value")
        with mock.patch.object(
            PUBLISH.os, "open", side_effect=OSError("fixture unavailable")
        ), self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "cannot open"):
            PUBLISH.sync_tree_directories(source)

    @unittest.skipUnless(sys.platform.startswith("linux"), "renameat2 requires Linux")
    def test_create_and_replace_are_whole_generation_swaps(self) -> None:
        destination = PUBLISH.DIST / "release"
        first = self.stage("first", "first")
        self.assertEqual(PUBLISH.publish(first, destination), "created")
        self.assertEqual(
            (destination / "value.txt").read_text(encoding="utf-8"), "first"
        )
        self.assertFalse(first.exists())

        second = self.stage("second", "second")
        self.assertEqual(PUBLISH.publish(second, destination), "replaced")
        self.assertEqual(
            (destination / "value.txt").read_text(encoding="utf-8"), "second"
        )
        self.assertEqual((second / "value.txt").read_text(encoding="utf-8"), "first")

    @unittest.skipUnless(sys.platform.startswith("linux"), "renameat2 requires Linux")
    def test_fsync_failure_restores_previous_generation(self) -> None:
        destination = PUBLISH.DIST / "evidence"
        first = self.stage("first", "first")
        PUBLISH.publish(first, destination)
        second = self.stage("second", "second")
        calls = 0

        def fail_once(source_parent_fd: int, destination_parent_fd: int) -> None:
            nonlocal calls
            calls += 1
            if calls == 1:
                raise OSError("fixture sync failure")
            PUBLISH.os.fsync(source_parent_fd)
            PUBLISH.os.fsync(destination_parent_fd)

        with mock.patch.object(PUBLISH, "sync_rename_parents", side_effect=fail_once):
            with self.assertRaisesRegex(OSError, "fixture sync failure"):
                PUBLISH.publish(second, destination)
        self.assertEqual(calls, 2)
        self.assertEqual(
            (destination / "value.txt").read_text(encoding="utf-8"), "first"
        )
        self.assertEqual((second / "value.txt").read_text(encoding="utf-8"), "second")

    @unittest.skipUnless(sys.platform.startswith("linux"), "renameat2 requires Linux")
    def test_fsync_failure_restores_unpublished_new_generation(self) -> None:
        destination = PUBLISH.DIST / "release"
        source = self.stage("new", "new")
        calls = 0

        def fail_once(source_parent_fd: int, destination_parent_fd: int) -> None:
            nonlocal calls
            calls += 1
            if calls == 1:
                raise OSError("fixture sync failure")
            PUBLISH.os.fsync(source_parent_fd)
            PUBLISH.os.fsync(destination_parent_fd)

        with mock.patch.object(PUBLISH, "sync_rename_parents", side_effect=fail_once):
            with self.assertRaisesRegex(OSError, "fixture sync failure"):
                PUBLISH.publish(source, destination)
        self.assertEqual(calls, 2)
        self.assertFalse(destination.exists())
        self.assertEqual((source / "value.txt").read_text(encoding="utf-8"), "new")

    def test_rejects_unsafe_boundaries_and_trees(self) -> None:
        source = self.stage("safe", "value")
        with self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "destination"):
            PUBLISH.publish(source, PUBLISH.DIST / "other")
        outside = PUBLISH.ROOT / "outside"
        outside.mkdir()
        with self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "source"):
            PUBLISH.publish(outside, PUBLISH.DIST / "release")
        if hasattr(os, "symlink"):
            (source / "link").symlink_to(source / "value.txt")
            with self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "unsafe file"):
                PUBLISH.publish(source, PUBLISH.DIST / "release")

    def test_tree_and_renameat2_primitives_fail_closed(self) -> None:
        missing = PUBLISH.ROOT / "missing"
        with self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "is missing"):
            PUBLISH.require_real_tree(missing, "fixture")
        ordinary = PUBLISH.ROOT / "ordinary"
        ordinary.write_text("not a directory", encoding="utf-8")
        with self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "real directory"):
            PUBLISH.require_real_tree(ordinary, "fixture")

        if hasattr(os, "symlink"):
            tree = PUBLISH.ROOT / "tree"
            target = PUBLISH.ROOT / "target"
            tree.mkdir()
            target.mkdir()
            (tree / "linked-directory").symlink_to(target, target_is_directory=True)
            with self.assertRaisesRegex(
                PUBLISH.PublishDirectoryError, "unsafe directory"
            ):
                PUBLISH.require_real_tree(tree, "fixture")

        with mock.patch.object(PUBLISH.os, "fsync") as fsync:
            PUBLISH.sync_rename_parents(11, 12)
        self.assertEqual([item.args[0] for item in fsync.call_args_list], [11, 12])

        unavailable = object()
        with mock.patch.object(
            PUBLISH.ctypes, "CDLL", return_value=unavailable
        ), self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "unavailable"):
            PUBLISH.renameat2(1, "source", 2, "destination", PUBLISH.RENAME_NOREPLACE)

        rename = mock.Mock(return_value=0)
        library = mock.Mock()
        library.renameat2 = rename
        with mock.patch.object(PUBLISH.ctypes, "CDLL", return_value=library):
            PUBLISH.renameat2(1, "source", 2, "destination", PUBLISH.RENAME_EXCHANGE)
        rename.assert_called_once_with(
            1,
            os.fsencode("source"),
            2,
            os.fsencode("destination"),
            PUBLISH.RENAME_EXCHANGE,
        )

        rename = mock.Mock(return_value=-1)
        library.renameat2 = rename
        with mock.patch.object(
            PUBLISH.ctypes, "CDLL", return_value=library
        ), mock.patch.object(
            PUBLISH.ctypes, "get_errno", return_value=17
        ), self.assertRaises(
            OSError
        ) as raised:
            PUBLISH.renameat2(1, "source", 2, "destination", PUBLISH.RENAME_NOREPLACE)
        self.assertEqual(raised.exception.errno, 17)

    def test_publish_rejects_platform_fd_and_rollback_failures(self) -> None:
        source = self.stage("failures", "value")
        destination = PUBLISH.DIST / "release"
        with mock.patch.object(
            PUBLISH.sys, "platform", "darwin"
        ), self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "requires Linux"):
            PUBLISH.publish(source, destination)

        with mock.patch.object(PUBLISH, "sync_tree_directories"), mock.patch.object(
            PUBLISH.os, "open", side_effect=OSError("dist denied")
        ), self.assertRaisesRegex(PUBLISH.PublishDirectoryError, "dist is unavailable"):
            PUBLISH.publish(source, destination)

        with mock.patch.object(PUBLISH, "sync_tree_directories"), mock.patch.object(
            PUBLISH.os, "open", side_effect=[10, OSError("parent denied")]
        ), mock.patch.object(PUBLISH.os, "close") as close, self.assertRaisesRegex(
            PUBLISH.PublishDirectoryError, "source parent"
        ):
            PUBLISH.publish(source, destination)
        close.assert_called_once_with(10)

        destination.write_text("not a directory", encoding="utf-8")
        with self.assertRaisesRegex(
            PUBLISH.PublishDirectoryError, "absent or a real directory"
        ):
            PUBLISH.publish(source, destination)
        destination.unlink()

        with mock.patch.object(PUBLISH, "sync_tree_directories"), mock.patch.object(
            PUBLISH,
            "require_real_tree",
            side_effect=[None, OSError("post-publication verification")],
        ), mock.patch.object(
            PUBLISH.os, "open", side_effect=[20, 21]
        ), mock.patch.object(
            PUBLISH.os, "stat", side_effect=FileNotFoundError
        ), mock.patch.object(
            PUBLISH.os, "fsync"
        ), mock.patch.object(
            PUBLISH.os, "close"
        ) as close, mock.patch.object(
            PUBLISH, "renameat2", side_effect=[None, OSError("rollback failed")]
        ) as rename, self.assertRaisesRegex(
            PUBLISH.PublishDirectoryError, "rollback also failed"
        ):
            PUBLISH.publish(source, destination)
        self.assertEqual(rename.call_count, 2)
        self.assertEqual([item.args[0] for item in close.call_args_list], [21, 20])

        if hasattr(os, "symlink"):
            linked_parent = PUBLISH.DIST / ".rkc-linked"
            linked_parent.mkdir()
            linked_source = linked_parent / "source"
            linked_source.symlink_to(source, target_is_directory=True)
            with self.assertRaisesRegex(
                PUBLISH.PublishDirectoryError, "private dist staging"
            ):
                PUBLISH.publish(linked_source, destination)

    def test_main_reports_success_and_failures(self) -> None:
        source = Path("source")
        destination = Path("destination")
        argv = [
            "publish_directory.py",
            "--source",
            str(source),
            "--destination",
            str(destination),
        ]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            PUBLISH, "publish", return_value="created"
        ), mock.patch("sys.stdout", new=io.StringIO()) as stdout:
            self.assertEqual(PUBLISH.main(), 0)
            self.assertEqual(json.loads(stdout.getvalue())["publication"], "created")

        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            PUBLISH, "publish", side_effect=PUBLISH.PublishDirectoryError("blocked")
        ), mock.patch("sys.stderr", new=io.StringIO()) as stderr:
            self.assertEqual(PUBLISH.main(), 1)
            self.assertIn("publish directory: blocked", stderr.getvalue())

    @unittest.skipUnless(sys.platform.startswith("linux"), "renameat2 requires Linux")
    def test_explicit_repository_root_supports_immutable_helper(self) -> None:
        outer = PUBLISH.ROOT / "outer"
        (outer / "dist").mkdir(parents=True)
        source = outer / "dist" / ".rkc-fixture.explicit" / "release"
        source.mkdir(parents=True)
        (source / "value.txt").write_text("explicit", encoding="utf-8")
        destination = outer / "dist" / "release"
        self.assertEqual(
            PUBLISH.publish(source, destination, repository_root=outer), "created"
        )
        self.assertEqual(
            (destination / "value.txt").read_text(encoding="utf-8"), "explicit"
        )


if __name__ == "__main__":
    unittest.main()
