package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/builtinplugins"
)

type doctorCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Fatal       bool   `json:"fatal"`
	Remediation string `json:"remediation,omitempty"`
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "optional configuration file")
	repository := fs.String("repository", ".", "repository path to inspect")
	jsonOutput := fs.Bool("json", false, "print JSON")
	strict := fs.Bool("strict", false, "fail when optional capabilities are unavailable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("doctor does not accept positional arguments; use --repository %q", fs.Arg(0))
	}
	cfg, err := loadConfiguration(*configPath)
	var checks []doctorCheck
	if err != nil {
		checks = append(checks, doctorCheck{
			Name: "configuration", Status: "fail", Detail: err.Error(), Fatal: true,
			Remediation: "generate a known-good file with 'rkc init --path rkc.json', then pass it with --config",
		})
	} else {
		checks = append(checks, doctorCheck{Name: "configuration", Status: "pass", Detail: "schema " + cfg.SchemaVersion + ", digest " + cfg.Digest(), Fatal: true})
	}
	checks = append(checks, doctorCheck{Name: "rkc", Status: "pass", Detail: version, Fatal: true})
	checks = append(checks, doctorCheck{Name: "go-runtime", Status: "pass", Detail: runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH, Fatal: true})

	absolute, pathErr := filepath.Abs(*repository)
	if pathErr != nil {
		checks = append(checks, doctorCheck{Name: "repository", Status: "fail", Detail: pathErr.Error(), Fatal: true, Remediation: "pass an existing directory with --repository"})
	} else if info, statErr := os.Stat(absolute); statErr != nil || !info.IsDir() {
		detail := "not a directory"
		if statErr != nil {
			detail = statErr.Error()
		}
		checks = append(checks, doctorCheck{Name: "repository", Status: "fail", Detail: detail, Fatal: true, Remediation: "pass an existing directory with --repository"})
	} else {
		checks = append(checks, doctorCheck{Name: "repository", Status: "pass", Detail: absolute, Fatal: true})
	}

	temp, tempErr := os.MkdirTemp("", "rkc-doctor-")
	if tempErr != nil {
		checks = append(checks, doctorCheck{Name: "temporary-storage", Status: "fail", Detail: tempErr.Error(), Fatal: true, Remediation: "set TMPDIR to a private writable directory"})
	} else {
		defer os.RemoveAll(temp)
		path, materializeErr := builtinplugins.MaterializePython(temp)
		if materializeErr != nil {
			checks = append(checks, doctorCheck{Name: "builtin-python-plugin", Status: "fail", Detail: materializeErr.Error(), Fatal: false, Remediation: "reinstall RKC or use scan --no-python"})
		} else {
			checks = append(checks, doctorCheck{Name: "builtin-python-plugin", Status: "pass", Detail: path, Fatal: false})
		}
	}

	python := "python3"
	if err == nil && cfg.Plugins.PythonAST.Interpreter != "" {
		python = cfg.Plugins.PythonAST.Interpreter
	}
	pythonCheck := doctorCheck{Name: "python", Status: "skip", Detail: "not required because the Python adapter is disabled", Fatal: false}
	if err != nil {
		pythonCheck.Detail = "not evaluated because the configuration is invalid"
	} else if cfg.Plugins.Enabled && cfg.Plugins.PythonAST.Enabled {
		pythonCheck = pythonExecutableCheck(python)
		if pythonCheck.Status == "fail" {
			pythonCheck.Remediation = "install Python 3.11+ or use scan --no-python"
		}
	}
	checks = append(checks, pythonCheck)
	gitCheck := executableCheck("git", "git", "--version", false)
	if gitCheck.Status == "fail" {
		gitCheck.Remediation = "install Git before scanning remote repositories; local directories remain available"
	}
	checks = append(checks, gitCheck)
	checks = append(checks, pythonIsolationCheck(cfg, err))

	fatalFailures := 0
	warnings := 0
	for _, check := range checks {
		if check.Status == "fail" {
			if check.Fatal {
				fatalFailures++
			} else {
				warnings++
			}
		}
	}
	ok := fatalFailures == 0 && (!*strict || warnings == 0)
	result := map[string]any{"ok": ok, "strict": *strict, "fatal_failures": fatalFailures, "warnings": warnings, "checks": checks}
	if *jsonOutput {
		if err := writeJSONStdout(result); err != nil {
			return err
		}
	} else {
		for _, check := range checks {
			status := strings.ToUpper(check.Status)
			if check.Status == "fail" && !check.Fatal {
				status = "WARN"
			}
			fmt.Printf("%-5s %-24s %s\n", status, check.Name, check.Detail)
			if check.Remediation != "" {
				fmt.Printf("      %-24s %s\n", "fix", check.Remediation)
			}
		}
		fmt.Printf("Result: fatal=%d warnings=%d ok=%t strict=%t\n", fatalFailures, warnings, ok, *strict)
	}
	if fatalFailures > 0 {
		return fmt.Errorf("doctor found %d fatal problem(s)", fatalFailures)
	}
	if *strict && warnings > 0 {
		return fmt.Errorf("doctor found %d optional capability warning(s) in strict mode", warnings)
	}
	return nil
}

func pythonIsolationCheck(cfg Configuration, configurationErr error) doctorCheck {
	check := doctorCheck{Name: "python-isolation", Fatal: false}
	if configurationErr != nil {
		check.Status = "skip"
		check.Detail = "not evaluated because the configuration is invalid"
		return check
	}
	if !cfg.Plugins.Enabled || !cfg.Plugins.PythonAST.Enabled {
		check.Status = "pass"
		check.Detail = "Python adapter disabled by configuration; portable deterministic scan is available"
		return check
	}
	check.Remediation = "use scan --no-python, or run on Linux with a reachable user-systemd manager"
	if runtime.GOOS != "linux" {
		check.Status = "fail"
		check.Detail = "the fail-closed Python adapter isolation boundary is only implemented on Linux"
		return check
	}
	paths := make([]string, 0, 3)
	for _, executable := range []string{"systemd-run", "systemctl", "env"} {
		path, err := exec.LookPath(executable)
		if err != nil {
			check.Status = "fail"
			check.Detail = fmt.Sprintf("required isolation command %q is unavailable: %v", executable, err)
			return check
		}
		paths = append(paths, path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, paths[1], "--user", "show-environment").CombinedOutput()
	if err != nil {
		check.Status = "fail"
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		check.Detail = "user-systemd manager is unreachable: " + detail
		return check
	}
	check.Status = "pass"
	check.Detail = "Linux user-systemd manager reachable; isolation commands: " + strings.Join(paths, ", ")
	check.Remediation = ""
	return check
}

func executableCheck(name, executable, argument string, fatal bool) doctorCheck {
	path, err := exec.LookPath(executable)
	if err != nil {
		return doctorCheck{Name: name, Status: "fail", Detail: err.Error(), Fatal: fatal}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, argument).CombinedOutput()
	if err != nil {
		return doctorCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("%s: %v", strings.TrimSpace(string(output)), err), Fatal: fatal}
	}
	return doctorCheck{Name: name, Status: "pass", Detail: path + " | " + strings.TrimSpace(string(output)), Fatal: fatal}
}

func pythonExecutableCheck(executable string) doctorCheck {
	check := executableCheck("python", executable, "--version", false)
	if check.Status != "pass" {
		return check
	}
	separator := strings.LastIndex(check.Detail, " | ")
	if separator < 0 {
		check.Status = "fail"
		check.Detail = "Python version output was not recognized"
		return check
	}
	supported, err := pythonVersionSupported(check.Detail[separator+3:])
	if err != nil {
		check.Status = "fail"
		check.Detail = err.Error()
		return check
	}
	if !supported {
		check.Status = "fail"
		check.Detail += " | RKC requires Python 3.11 or newer"
	}
	return check
}

func pythonVersionSupported(output string) (bool, error) {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) < 2 || strings.ToLower(fields[0]) != "python" {
		return false, fmt.Errorf("unrecognized Python version output %q", strings.TrimSpace(output))
	}
	parts := strings.Split(fields[1], ".")
	if len(parts) < 2 {
		return false, fmt.Errorf("unrecognized Python version %q", fields[1])
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return false, fmt.Errorf("unrecognized Python version %q", fields[1])
	}
	return major > 3 || (major == 3 && minor >= 11), nil
}
