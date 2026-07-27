package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorExecutableChecksClassifyVersionsAndFailures(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, test := range []struct {
		name   string
		path   string
		status string
		text   string
	}{
		{"supported", write("supported", "printf 'Python 3.11.9\\n'"), "pass", "Python 3.11.9"},
		{"future major", write("future", "printf 'Python 4.0.0\\n'"), "pass", "Python 4.0.0"},
		{"old", write("old", "printf 'Python 3.10.14\\n'"), "fail", "requires Python 3.11"},
		{"unrecognized", write("bad", "printf 'not-python\\n'"), "fail", "unrecognized"},
		{"failure", write("failure", "printf 'fixture failure' >&2; exit 7"), "fail", "fixture failure"},
		{"missing", filepath.Join(root, "missing"), "fail", "no such file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := pythonExecutableCheck(test.path)
			if check.Status != test.status || !strings.Contains(strings.ToLower(check.Detail), strings.ToLower(test.text)) {
				t.Fatalf("pythonExecutableCheck(%q) = %+v", test.path, check)
			}
		})
	}
	if check := executableCheck("fixture", write("ok", "printf 'ok\\n'"), "--version", true); check.Status != "pass" || !check.Fatal {
		t.Fatalf("executableCheck success = %+v", check)
	}
}
