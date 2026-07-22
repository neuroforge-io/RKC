#!/usr/bin/env python3
"""Tests for atomic complete-generation publication."""
from __future__ import annotations

import importlib.util
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
            PUBLISH.os, "fsync", side_effect=lambda descriptor: synced.append(descriptor)
        ), mock.patch.object(PUBLISH.os, "close"):
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
        self.assertEqual((destination / "value.txt").read_text(encoding="utf-8"), "first")
        self.assertFalse(first.exists())

        second = self.stage("second", "second")
        self.assertEqual(PUBLISH.publish(second, destination), "replaced")
        self.assertEqual((destination / "value.txt").read_text(encoding="utf-8"), "second")
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
        self.assertEqual((destination / "value.txt").read_text(encoding="utf-8"), "first")
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


if __name__ == "__main__":
    unittest.main()
