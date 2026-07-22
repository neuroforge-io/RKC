from __future__ import annotations

import json
import io
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import coverage_gate


class CoverageGateTests(unittest.TestCase):
    def package(self, root: Path, import_path: str, relative: str) -> coverage_gate.GoPackage:
        source = root / relative
        source.parent.mkdir(parents=True, exist_ok=True)
        source.write_text("package fixture\nfunc covered() {}\n", encoding="utf-8")
        return coverage_gate.GoPackage(
            import_path=import_path,
            directory=source.parent,
            source_files=(source,),
            generated_files=(),
            ignored_files=(),
        )

    def test_json_stream_decodes_concatenated_objects(self) -> None:
        self.assertEqual(
            coverage_gate._json_stream('{"one": 1}\n {"two": 2}\n'),
            [{"one": 1}, {"two": 2}],
        )
        with self.assertRaises(coverage_gate.GateError):
            coverage_gate._json_stream('{"one": 1}\nnot-json')

    def test_go_profile_deduplicates_blocks_with_covered_or_semantics(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = self.package(
                root,
                "example.test/rkc/internal/fixture",
                "internal/fixture/a.go",
            )
            profile = root / "raw.out"
            profile.write_text(
                "mode: atomic\n"
                "example.test/rkc/internal/fixture/a.go:2.1,2.18 3 0\n"
                "example.test/rkc/internal/fixture/a.go:2.1,2.18 3 7\n"
                "example.test/rkc/internal/fixture/a.go:2.18,2.18 0 4\n",
                encoding="utf-8",
            )
            report = coverage_gate.parse_go_profile(
                profile,
                "example.test/rkc",
                [package],
                root,
            )
            self.assertEqual(report["raw_records"], 3)
            self.assertEqual(report["unique_blocks"], 2)
            self.assertEqual(report["deduplicated_records"], 1)
            self.assertEqual(report["statements"], 3)
            self.assertEqual(report["covered_statements"], 3)
            self.assertEqual(report["percent"], 100.0)

    def test_go_profile_rejects_unowned_source(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = self.package(root, "example.test/rkc/p", "p/a.go")
            profile = root / "raw.out"
            profile.write_text(
                "mode: atomic\nexample.test/other/escape.go:1.1,1.2 1 1\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(coverage_gate.GateError, "unowned"):
                coverage_gate.parse_go_profile(profile, "example.test/rkc", [package], root)

    def test_go_profile_rejects_conflicting_duplicate_denominator(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = self.package(root, "example.test/rkc/p", "p/a.go")
            profile = root / "raw.out"
            profile.write_text(
                "mode: atomic\n"
                "example.test/rkc/p/a.go:2.1,2.18 1 0\n"
                "example.test/rkc/p/a.go:2.1,2.18 2 1\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(coverage_gate.GateError, "statement count"):
                coverage_gate.parse_go_profile(profile, "example.test/rkc", [package], root)

    def test_go_threshold_checks_every_executable_package(self) -> None:
        report = {
            "percent": 95.0,
            "packages": [
                {
                    "package": "example.test/large",
                    "percent": 99.0,
                    "threshold_applicable": True,
                },
                {
                    "package": "example.test/small",
                    "percent": 50.0,
                    "threshold_applicable": True,
                },
                {
                    "package": "example.test/types-only",
                    "percent": 100.0,
                    "threshold_applicable": False,
                },
            ],
        }
        failures = coverage_gate.evaluate_go(
            report,
            overall_minimum=90.0,
            package_minimum=80.0,
        )
        self.assertEqual(len(failures), 1)
        self.assertIn("example.test/small", failures[0])

    def test_python_report_uses_lines_plus_branches_and_requires_inventory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "python.json"
            path.write_text(
                json.dumps(
                    {
                        "meta": {"branch_coverage": True},
                        "files": {
                            "scripts/example.py": {
                                "summary": {
                                    "num_statements": 4,
                                    "covered_lines": 3,
                                    "num_branches": 2,
                                    "covered_branches": 1,
                                }
                            }
                        },
                    }
                ),
                encoding="utf-8",
            )
            report = coverage_gate.parse_python_report(path, ["scripts/example.py"])
            self.assertEqual(report["units"], 6)
            self.assertEqual(report["covered_units"], 4)
            self.assertAlmostEqual(float(report["percent"]), 66.6667, places=4)
            failures = coverage_gate.evaluate_python(
                report,
                overall_minimum=60.0,
                file_minimum=80.0,
            )
            self.assertEqual(len(failures), 1)
            self.assertIn("scripts/example.py", failures[0])
            with self.assertRaisesRegex(coverage_gate.GateError, "omitted"):
                coverage_gate.parse_python_report(
                    path,
                    ["scripts/example.py", "scripts/missing.py"],
                )

    def test_python_report_rejects_line_only_measurement(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "python.json"
            path.write_text(
                json.dumps({"meta": {"branch_coverage": False}, "files": {}}),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(coverage_gate.GateError, "branch coverage"):
                coverage_gate.parse_python_report(path, [])

    @mock.patch("coverage_gate.subprocess.run")
    def test_visible_subprocess_failure_is_returned_unchanged(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(["fixture"], 17)
        with mock.patch("sys.stdout", new=io.StringIO()), mock.patch(
            "sys.stderr", new=io.StringIO()
        ):
            self.assertEqual(coverage_gate.run_visible(["fixture"]), 17)
        self.assertFalse(run.call_args.kwargs["check"])

    @mock.patch("coverage_gate.subprocess.run")
    def test_logged_subprocess_retains_failure_status_and_output(
        self, run: mock.Mock
    ) -> None:
        def fake_run(*_args: object, **kwargs: object) -> subprocess.CompletedProcess[str]:
            output = kwargs["stdout"]
            output.write("actionable failure\n")
            return subprocess.CompletedProcess(["fixture"], 9)

        run.side_effect = fake_run
        with tempfile.TemporaryDirectory() as temporary, mock.patch(
            "sys.stdout", new=io.StringIO()
        ), mock.patch("sys.stderr", new=io.StringIO()) as errors:
            log = Path(temporary) / "child.log"
            self.assertEqual(coverage_gate.run_logged(["fixture"], log), 9)
            self.assertEqual(log.read_text(encoding="utf-8"), "actionable failure\n")
            self.assertIn("actionable failure", errors.getvalue())
        self.assertFalse(run.call_args.kwargs["check"])

    def test_output_directory_rejects_symlink(self) -> None:
        if not hasattr(os, "symlink"):
            self.skipTest("symlink support required")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            destination = root / "real"
            destination.mkdir()
            link = root / "output"
            link.symlink_to(destination, target_is_directory=True)
            with self.assertRaisesRegex(coverage_gate.GateError, "real directory"):
                coverage_gate._make_run_directory(link)

    def test_policy_minimum_rejects_out_of_range_values(self) -> None:
        self.assertEqual(coverage_gate._validate_minimum(80.0, "fixture"), 80.0)
        for invalid in (-0.1, 100.1):
            with self.assertRaises(Exception):
                coverage_gate._validate_minimum(invalid, "fixture")

    def test_relative_generated_and_profile_helpers(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "generated.go"
            source.write_text(
                "// header\n// Code generated fixture. DO NOT EDIT.\npackage fixture\n",
                encoding="utf-8",
            )
            self.assertEqual(coverage_gate._relative(source, root), "generated.go")
            self.assertTrue(coverage_gate._is_generated(source, coverage_gate.GENERATED_RE))
            source.write_text("package fixture\n", encoding="utf-8")
            self.assertFalse(coverage_gate._is_generated(source, coverage_gate.GENERATED_RE))
            with self.assertRaisesRegex(coverage_gate.GateError, "escapes"):
                coverage_gate._relative(Path(temporary).parent / "outside", root)

    @mock.patch("coverage_gate.subprocess.run")
    def test_discover_go_packages_inventories_generated_and_ignored(
        self, run: mock.Mock
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            package_dir = root / "pkg"
            package_dir.mkdir()
            (package_dir / "current.go").write_text("package pkg\n", encoding="utf-8")
            (package_dir / "generated.go").write_text(
                "// Code generated fixture. DO NOT EDIT.\npackage pkg\n", encoding="utf-8"
            )
            (package_dir / "windows.go").write_text("package pkg\n", encoding="utf-8")
            document = {
                "ImportPath": "example.test/rkc/pkg",
                "Dir": str(package_dir),
                "GoFiles": ["current.go", "generated.go"],
                "IgnoredGoFiles": ["windows.go"],
                "Module": {"Main": True, "Path": "example.test/rkc"},
            }
            run.return_value = subprocess.CompletedProcess(
                [], 0, stdout=json.dumps(document), stderr=""
            )
            module, packages = coverage_gate.discover_go_packages(root)
            self.assertEqual(module, "example.test/rkc")
            self.assertEqual(packages[0].source_files, (package_dir / "current.go",))
            self.assertEqual(packages[0].generated_files, (package_dir / "generated.go",))
            self.assertEqual(packages[0].ignored_files, (package_dir / "windows.go",))
            document["Module"] = {"Main": False, "Path": "example.test/rkc"}
            run.return_value = subprocess.CompletedProcess(
                [], 0, stdout=json.dumps(document), stderr=""
            )
            with self.assertRaisesRegex(coverage_gate.GateError, "non-main"):
                coverage_gate.discover_go_packages(root)
            run.return_value = subprocess.CompletedProcess([], 2, stdout="", stderr="go failed")
            with self.assertRaisesRegex(coverage_gate.GateError, "go failed"):
                coverage_gate.discover_go_packages(root)

    def test_profile_validation_and_stable_merged_output(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = self.package(root, "example.test/rkc/p", "p/a.go")
            generated = root / "p/generated.go"
            generated.write_text(
                "// Code generated fixture. DO NOT EDIT.\npackage p\n", encoding="utf-8"
            )
            package = coverage_gate.GoPackage(
                package.import_path,
                package.directory,
                package.source_files,
                (generated,),
                (),
            )
            profile = root / "raw.out"
            profile.write_text(
                "mode: count\n"
                "example.test/rkc/p/a.go:2.1,2.18 1 0\n"
                "example.test/rkc/p/generated.go:2.1,2.10 1 1\n",
                encoding="utf-8",
            )
            report = coverage_gate.parse_go_profile(
                profile, "example.test/rkc", [package], root
            )
            self.assertEqual(report["generated_records_excluded"], 1)
            self.assertEqual(report["percent"], 0.0)
            merged = root / "merged.out"
            coverage_gate.write_merged_go_profile(merged, report["blocks"])
            self.assertTrue(merged.read_text(encoding="utf-8").startswith("mode: set\n"))
            for payload, marker in (
                ("", "mode header"),
                ("mode: weird\n", "unsupported"),
                ("mode: set\nbad row\n", "invalid"),
                ("mode: set\nexample.test/rkc/p/a.go:3.2,2.1 1 1\n", "reversed"),
                ("mode: set\nexample.test/rkc/p/a.go:2.1,2.2 0 1\n", "zero executable"),
            ):
                profile.write_text(payload, encoding="utf-8")
                with self.subTest(marker=marker), self.assertRaisesRegex(
                    coverage_gate.GateError, marker
                ):
                    coverage_gate.parse_go_profile(
                        profile, "example.test/rkc", [package], root
                    )

    def test_evaluators_cover_overall_missing_and_non_applicable_rows(self) -> None:
        self.assertEqual(
            coverage_gate.evaluate_go(
                {"percent": 100.0}, overall_minimum=90, package_minimum=80
            ),
            ["Go report has no package metrics"],
        )
        failures = coverage_gate.evaluate_go(
            {"percent": 50.0, "packages": [None, {"threshold_applicable": False}]},
            overall_minimum=90,
            package_minimum=80,
        )
        self.assertEqual(len(failures), 1)
        self.assertIn("overall", failures[0])
        self.assertEqual(
            coverage_gate.evaluate_python(
                {"percent": 100.0}, overall_minimum=90, file_minimum=80
            ),
            ["Python report has no file metrics"],
        )
        failures = coverage_gate.evaluate_python(
            {
                "percent": 50.0,
                "files": [
                    None,
                    {"threshold_applicable": False},
                    {"threshold_applicable": True, "file": "x.py", "percent": 10},
                ],
            },
            overall_minimum=90,
            file_minimum=80,
        )
        self.assertEqual(len(failures), 2)

    def test_python_inventory_and_report_counter_failures(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for directory in ("internal", "plugins", "scripts"):
                (root / directory).mkdir()
            (root / "scripts/main.py").write_text("x = 1\n", encoding="utf-8")
            (root / "scripts/test_main.py").write_text("pass\n", encoding="utf-8")
            (root / "plugins/generated.py").write_text(
                "# Code generated fixture. DO NOT EDIT.\n", encoding="utf-8"
            )
            self.assertEqual(coverage_gate.discover_python_sources(root), ["scripts/main.py"])
            report = root / "report.json"
            base = {"meta": {"branch_coverage": True}, "files": {}}
            for files, marker in (
                ({"unexpected.py": {"summary": {}}}, "omitted"),
                (
                    {
                        "scripts/main.py": {"summary": {}},
                        "unexpected.py": {"summary": {}},
                    },
                    "non-production",
                ),
                ({"scripts/main.py": {}}, "no summary"),
                (
                    {
                        "scripts/main.py": {
                            "summary": {
                                "num_statements": -1,
                                "covered_lines": 0,
                                "num_branches": 0,
                                "covered_branches": 0,
                            }
                        }
                    },
                    "invalid counters",
                ),
                (
                    {
                        "scripts/main.py": {
                            "summary": {
                                "num_statements": 1,
                                "covered_lines": 2,
                                "num_branches": 0,
                                "covered_branches": 0,
                            }
                        }
                    },
                    "exceeds",
                ),
                (
                    {
                        "scripts/main.py": {
                            "summary": {
                                "num_statements": 0,
                                "covered_lines": 0,
                                "num_branches": 0,
                                "covered_branches": 0,
                            }
                        }
                    },
                    "zero executable",
                ),
            ):
                base["files"] = files
                report.write_text(json.dumps(base), encoding="utf-8")
                with self.subTest(marker=marker), self.assertRaisesRegex(
                    coverage_gate.GateError, marker
                ):
                    coverage_gate.parse_python_report(report, ["scripts/main.py"])

    @mock.patch("coverage_gate.subprocess.run")
    def test_subprocess_helpers_configuration_and_commands(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess([], 0)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "stdout.txt"
            self.assertEqual(
                coverage_gate.run_visible(
                    ["fixture"],
                    cwd=root,
                    environment={"A": "B"},
                    input_text="input",
                    stdout_path=output,
                ),
                0,
            )
            self.assertTrue(output.exists())
            log = root / "log"
            self.assertEqual(coverage_gate.run_logged(["fixture"], log, cwd=root), 0)
            config = root / "coverage.ini"
            coverage_gate.write_coverage_config(config, root / ".coverage")
            self.assertIn("branch = True", config.read_text(encoding="utf-8"))
            command = coverage_gate._coverage_command("python", config, ["script.py"])
            self.assertIn("--parallel-mode", command)
            commands = coverage_gate.python_commands(coverage_gate.ROOT)
            self.assertTrue(
                any(
                    any(argument.endswith("package-complete.py") for argument in item.arguments)
                    for item in commands
                )
            )
            self.assertIn("sample-python", coverage_gate._extractor_request(coverage_gate.ROOT))

    def test_run_directory_parser_and_summary(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "coverage"
            run = coverage_gate._make_run_directory(output)
            self.assertTrue(run.is_dir())
            parser = coverage_gate.build_parser()
            arguments = parser.parse_args(["--python-file-min", "81"])
            self.assertEqual(arguments.python_file_min, 81)
            self.assertEqual(arguments.python, coverage_gate.preferred_python())
            go_report = {
                "percent": 100,
                "covered_statements": 1,
                "statements": 1,
                "packages": [
                    {"package": "p", "threshold_applicable": True, "percent": 100},
                    {"package": "types", "threshold_applicable": False, "percent": 100},
                ],
            }
            python_report = {
                "percent": 100,
                "covered_units": 1,
                "units": 1,
                "files": [
                    {"file": "x.py", "threshold_applicable": True, "percent": 100},
                    {"file": "empty.py", "threshold_applicable": False, "percent": 100},
                ],
            }
            with mock.patch("sys.stdout", new=io.StringIO()) as stdout:
                coverage_gate._print_summary(go_report, python_report)
                self.assertIn("no executable", stdout.getvalue())
            bad = Path(temporary) / "file"
            bad.write_text("x", encoding="utf-8")
            with self.assertRaises(coverage_gate.GateError):
                coverage_gate._make_run_directory(bad)

    def test_preferred_python_uses_locked_venv_and_has_safe_fallback(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.assertEqual(coverage_gate.preferred_python(root), sys.executable)
            relative = (
                Path("Scripts/python.exe")
                if coverage_gate.os.name == "nt"
                else Path("bin/python")
            )
            interpreter = root / ".venv" / relative
            interpreter.parent.mkdir(parents=True)
            interpreter.write_bytes(b"fixture")
            interpreter.chmod(0o700)
            self.assertEqual(
                coverage_gate.preferred_python(root), str(interpreter.absolute())
            )

    def test_main_orchestration_success_policy_failure_and_gate_error(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run_directory = root / "run"
            run_directory.mkdir()
            source = root / "p.go"
            source.write_text("package p\n", encoding="utf-8")
            package = coverage_gate.GoPackage("example.test/rkc/p", root, (source,), (), ())
            block = coverage_gate.GoBlock("p.go", 1, 1, 1, 2, 1, True)
            go_report = {
                "percent": 100.0,
                "covered_statements": 1,
                "statements": 1,
                "packages": [
                    {
                        "package": package.import_path,
                        "threshold_applicable": True,
                        "percent": 100.0,
                    }
                ],
                "blocks": [block],
            }
            python_report = {
                "percent": 100.0,
                "covered_units": 1,
                "units": 1,
                "files": [
                    {
                        "file": "scripts/x.py",
                        "threshold_applicable": True,
                        "percent": 100.0,
                    }
                ],
            }
            patches = (
                mock.patch.object(coverage_gate, "_make_run_directory", return_value=run_directory),
                mock.patch.object(
                    coverage_gate,
                    "discover_go_packages",
                    return_value=("example.test/rkc", [package]),
                ),
                mock.patch.object(coverage_gate, "run_logged", return_value=0),
                mock.patch.object(coverage_gate, "parse_go_profile", return_value=go_report),
                mock.patch.object(
                    coverage_gate, "discover_python_sources", return_value=["scripts/x.py"]
                ),
                mock.patch.object(coverage_gate, "python_commands", return_value=[]),
                mock.patch.object(coverage_gate, "run_visible", return_value=0),
                mock.patch.object(
                    coverage_gate, "parse_python_report", return_value=python_report
                ),
            )
            with patches[0], patches[1], patches[2], patches[3], patches[4], patches[5], patches[6], patches[7], mock.patch(
                "sys.stdout", new=io.StringIO()
            ):
                self.assertEqual(coverage_gate.main([]), 0)
            self.assertTrue((run_directory / "summary.json").is_file())

            failing_run = root / "failing-run"
            failing_run.mkdir()
            failing_go_report = {
                **{key: value for key, value in go_report.items() if key != "blocks"},
                "blocks": [block],
            }

            def visible(
                arguments: list[str],
                **kwargs: object,
            ) -> int:
                captured = kwargs.get("stdout_path")
                if isinstance(captured, Path):
                    captured.write_text('{"ok":true}', encoding="utf-8")
                if "failing-command" in arguments:
                    return 9
                return 0

            commands = [
                coverage_gate.CommandSpec(("captured-command",), capture_json=True),
                coverage_gate.CommandSpec(("failing-command",)),
            ]
            with mock.patch.object(
                coverage_gate, "_make_run_directory", return_value=failing_run
            ), mock.patch.object(
                coverage_gate,
                "discover_go_packages",
                return_value=("example.test/rkc", [package]),
            ), mock.patch.object(coverage_gate, "run_logged", return_value=7), mock.patch.object(
                coverage_gate, "parse_go_profile", return_value=failing_go_report
            ), mock.patch.object(
                coverage_gate, "discover_python_sources", return_value=["scripts/x.py"]
            ), mock.patch.object(
                coverage_gate, "python_commands", return_value=commands
            ), mock.patch.object(
                coverage_gate, "run_visible", side_effect=visible
            ), mock.patch.object(
                coverage_gate, "parse_python_report", return_value=python_report
            ), mock.patch("sys.stdout", new=io.StringIO()), mock.patch(
                "sys.stderr", new=io.StringIO()
            ):
                self.assertEqual(coverage_gate.main([]), 1)
            evidence = json.loads((failing_run / "summary.json").read_text(encoding="utf-8"))
            self.assertFalse(evidence["ok"])
            self.assertTrue(any("subprocess" in item for item in evidence["failures"]))

        with mock.patch.object(
            coverage_gate, "_make_run_directory", side_effect=coverage_gate.GateError("blocked")
        ), mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(coverage_gate.main([]), 1)
        with mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(coverage_gate.main(["--go-overall-min", "101"]), 2)


if __name__ == "__main__":
    unittest.main()
