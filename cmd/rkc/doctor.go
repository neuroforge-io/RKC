package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/builtinplugins"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fatal  bool   `json:"fatal"`
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "optional configuration file")
	repository := fs.String("repository", ".", "repository path to inspect")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfiguration(*configPath)
	var checks []doctorCheck
	if err != nil {
		checks = append(checks, doctorCheck{Name: "configuration", Status: "fail", Detail: err.Error(), Fatal: true})
	} else {
		checks = append(checks, doctorCheck{Name: "configuration", Status: "pass", Detail: "schema " + cfg.SchemaVersion + ", digest " + cfg.Digest(), Fatal: true})
	}
	checks = append(checks, doctorCheck{Name: "rkc", Status: "pass", Detail: version, Fatal: true})
	checks = append(checks, doctorCheck{Name: "go-runtime", Status: "pass", Detail: runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH, Fatal: true})

	absolute, pathErr := filepath.Abs(*repository)
	if pathErr != nil {
		checks = append(checks, doctorCheck{Name: "repository", Status: "fail", Detail: pathErr.Error(), Fatal: true})
	} else if info, statErr := os.Stat(absolute); statErr != nil || !info.IsDir() {
		detail := "not a directory"
		if statErr != nil {
			detail = statErr.Error()
		}
		checks = append(checks, doctorCheck{Name: "repository", Status: "fail", Detail: detail, Fatal: true})
	} else {
		checks = append(checks, doctorCheck{Name: "repository", Status: "pass", Detail: absolute, Fatal: true})
	}

	temp, tempErr := os.MkdirTemp("", "rkc-doctor-")
	if tempErr != nil {
		checks = append(checks, doctorCheck{Name: "temporary-storage", Status: "fail", Detail: tempErr.Error(), Fatal: true})
	} else {
		defer os.RemoveAll(temp)
		path, materializeErr := builtinplugins.MaterializePython(temp)
		if materializeErr != nil {
			checks = append(checks, doctorCheck{Name: "builtin-python-plugin", Status: "fail", Detail: materializeErr.Error(), Fatal: false})
		} else {
			checks = append(checks, doctorCheck{Name: "builtin-python-plugin", Status: "pass", Detail: path, Fatal: false})
		}
	}

	python := "python3"
	if err == nil && cfg.Plugins.PythonAST.Interpreter != "" {
		python = cfg.Plugins.PythonAST.Interpreter
	}
	checks = append(checks, executableCheck("python", python, "--version", false))
	checks = append(checks, executableCheck("git", "git", "--version", false))

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
	result := map[string]any{"ok": fatalFailures == 0, "fatal_failures": fatalFailures, "warnings": warnings, "checks": checks}
	if *jsonOutput {
		if err := writeJSONStdout(result); err != nil {
			return err
		}
	} else {
		for _, check := range checks {
			fmt.Printf("%-5s %-24s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail)
		}
		fmt.Printf("Result: fatal=%d warnings=%d\n", fatalFailures, warnings)
	}
	if fatalFailures > 0 {
		return fmt.Errorf("doctor found %d fatal problem(s)", fatalFailures)
	}
	return nil
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
