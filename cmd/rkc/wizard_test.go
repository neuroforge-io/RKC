package main

import (
	"bufio"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

type wizardCalls struct {
	opened    [][]string
	compiled  [][]string
	helpCalls int
}

func (calls *wizardCalls) actions() wizardActions {
	return wizardActions{
		Open: func(args []string) error {
			calls.opened = append(calls.opened, append([]string(nil), args...))
			return nil
		},
		Quickstart: func(args []string) error {
			calls.compiled = append(calls.compiled, append([]string(nil), args...))
			return nil
		},
		Help: func(output io.Writer) error {
			calls.helpCalls++
			_, err := io.WriteString(output, "complete help\n")
			return err
		},
	}
}

func TestWizardDefaultBuildsAndOpensPromptedFolder(t *testing.T) {
	calls := &wizardCalls{}
	var output strings.Builder
	if err := runWizardWith(nil, strings.NewReader("  /repo with spaces  \n\n"), &output, calls.actions()); err != nil {
		t.Fatal(err)
	}
	if len(calls.opened) != 1 || len(calls.opened[0]) != 2 || calls.opened[0][0] != "--" || calls.opened[0][1] != "/repo with spaces" {
		t.Fatalf("open calls = %#v", calls.opened)
	}
	if len(calls.compiled) != 0 || calls.helpCalls != 0 {
		t.Fatalf("unexpected alternate actions: compile=%#v help=%d", calls.compiled, calls.helpCalls)
	}
	for _, want := range []string{"RKC guided first run", "read-only browser atlas", "Press Ctrl-C"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("wizard output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestWizardCompileHelpInvalidChoiceAndCancel(t *testing.T) {
	t.Run("compile positional folder", func(t *testing.T) {
		calls := &wizardCalls{}
		var output strings.Builder
		if err := runWizardWith([]string{"relative folder"}, strings.NewReader("compile\n"), &output, calls.actions()); err != nil {
			t.Fatal(err)
		}
		if len(calls.compiled) != 1 || len(calls.compiled[0]) != 2 || calls.compiled[0][0] != "--" || calls.compiled[0][1] != "relative folder" || len(calls.opened) != 0 {
			t.Fatalf("calls = open %#v compile %#v", calls.opened, calls.compiled)
		}
		if !strings.Contains(output.String(), "without starting a server") {
			t.Fatalf("compile summary missing: %s", output.String())
		}
	})

	t.Run("help", func(t *testing.T) {
		calls := &wizardCalls{}
		var output strings.Builder
		if err := runWizardWith([]string{"."}, strings.NewReader("3\n"), &output, calls.actions()); err != nil {
			t.Fatal(err)
		}
		if calls.helpCalls != 1 || len(calls.opened) != 0 || len(calls.compiled) != 0 {
			t.Fatalf("calls = %#v", calls)
		}
		if !strings.Contains(output.String(), "complete help") {
			t.Fatalf("help output missing: %s", output.String())
		}
	})

	t.Run("invalid then cancel", func(t *testing.T) {
		calls := &wizardCalls{}
		var output strings.Builder
		if err := runWizardWith([]string{"."}, strings.NewReader("something else\nq\n"), &output, calls.actions()); err != nil {
			t.Fatal(err)
		}
		if len(calls.opened) != 0 || len(calls.compiled) != 0 || calls.helpCalls != 0 {
			t.Fatalf("cancel started an action: %#v", calls)
		}
		for _, want := range []string{"Please enter 1, 2, 3, or q.", "Cancelled; nothing was started."} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("cancel output does not contain %q: %s", want, output.String())
			}
		}
	})
}

func TestWizardEOFDoesNotStartWork(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		input string
		want  string
	}{
		{name: "before folder", want: "No input received"},
		{name: "before choice", args: []string{"."}, want: "No choice received"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := &wizardCalls{}
			var output strings.Builder
			if err := runWizardWith(test.args, strings.NewReader(test.input), &output, calls.actions()); err != nil {
				t.Fatal(err)
			}
			if len(calls.opened) != 0 || len(calls.compiled) != 0 || calls.helpCalls != 0 {
				t.Fatalf("EOF started an action: %#v", calls)
			}
			if !strings.Contains(output.String(), test.want+"; nothing was started.") {
				t.Fatalf("EOF output = %q", output.String())
			}
		})
	}
}

func TestWizardDefaultsBlankFolderAndDispatchesAliases(t *testing.T) {
	calls := &wizardCalls{}
	var output strings.Builder
	if err := runWizardWith(nil, strings.NewReader("\n2\n"), &output, calls.actions()); err != nil {
		t.Fatal(err)
	}
	if len(calls.compiled) != 1 || len(calls.compiled[0]) != 2 || calls.compiled[0][0] != "--" || calls.compiled[0][1] != "." {
		t.Fatalf("blank-folder compile calls = %#v", calls.compiled)
	}
	flagLikeCalls := &wizardCalls{}
	if err := runWizardWith([]string{"--", "-repository"}, strings.NewReader("2\n"), io.Discard, flagLikeCalls.actions()); err != nil {
		t.Fatal(err)
	}
	if len(flagLikeCalls.compiled) != 1 || len(flagLikeCalls.compiled[0]) != 2 || flagLikeCalls.compiled[0][0] != "--" || flagLikeCalls.compiled[0][1] != "-repository" {
		t.Fatalf("flag-like folder compile calls = %#v", flagLikeCalls.compiled)
	}
	for _, alias := range []string{"wizard", "tui"} {
		captured, err := captureStdout(t, func() error { return run([]string{alias, "--help"}) })
		if err != nil {
			t.Fatalf("%s --help: %v", alias, err)
		}
		if !strings.Contains(captured, "safe first run") || !strings.Contains(captured, "not a replacement") {
			t.Fatalf("%s --help output = %q", alias, captured)
		}
	}
}

func TestWizardRejectsInvalidSetupAndArguments(t *testing.T) {
	valid := (&wizardCalls{}).actions()
	if err := runWizardWith(nil, nil, io.Discard, valid); err == nil || !strings.Contains(err.Error(), "input and output") {
		t.Fatalf("nil input error = %v", err)
	}
	if err := runWizardWith(nil, strings.NewReader(""), nil, valid); err == nil || !strings.Contains(err.Error(), "input and output") {
		t.Fatalf("nil output error = %v", err)
	}
	if err := runWizardWith(nil, strings.NewReader(""), io.Discard, wizardActions{}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing actions error = %v", err)
	}
	if err := runWizardWith([]string{"one", "two"}, strings.NewReader(""), io.Discard, valid); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("extra folder error = %v", err)
	}
	if err := runWizardWith([]string{"--unknown"}, strings.NewReader(""), io.Discard, valid); err == nil {
		t.Fatal("unknown wizard option succeeded")
	}
	var help strings.Builder
	if err := runWizardWith([]string{"--help"}, strings.NewReader(""), &help, valid); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("wizard --help error = %v", err)
	}
	if !strings.Contains(help.String(), "rkc tui") {
		t.Fatalf("wizard help = %q", help.String())
	}
}

func TestWizardPropagatesActionAndInputErrors(t *testing.T) {
	want := errors.New("sentinel")
	tests := []struct {
		name    string
		input   string
		mutate  func(*wizardActions)
		message string
	}{
		{name: "open", input: "1\n", mutate: func(actions *wizardActions) { actions.Open = func([]string) error { return want } }, message: "wizard open"},
		{name: "compile", input: "2\n", mutate: func(actions *wizardActions) { actions.Quickstart = func([]string) error { return want } }, message: "wizard compile"},
		{name: "help", input: "3\n", mutate: func(actions *wizardActions) { actions.Help = func(io.Writer) error { return want } }, message: "write wizard help"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actions := (&wizardCalls{}).actions()
			test.mutate(&actions)
			err := runWizardWith([]string{"."}, strings.NewReader(test.input), io.Discard, actions)
			if !errors.Is(err, want) || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	tooLong := strings.Repeat("x", wizardMaximumInputBytes+1)
	err := runWizardWith(nil, strings.NewReader(tooLong), io.Discard, (&wizardCalls{}).actions())
	if err == nil || !strings.Contains(err.Error(), "read wizard folder") {
		t.Fatalf("oversized input error = %v", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(""))
	if value, ok, err := readWizardLine(scanner, io.Discard, "prompt"); err != nil || ok || value != "" {
		t.Fatalf("direct EOF = value %q ok %t err %v", value, ok, err)
	}
}

type wizardFailWriter struct {
	failAt int
	writes int
}

func (writer *wizardFailWriter) Write(value []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, errors.New("writer failed")
	}
	return len(value), nil
}

func TestWizardReportsOutputFailuresBeforeStartingActions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		input   string
		failAt  int
		message string
	}{
		{name: "introduction", args: []string{"."}, input: "1\n", failAt: 1, message: "write wizard introduction"},
		{name: "folder prompt", input: ".\n1\n", failAt: 2, message: "read wizard folder"},
		{name: "choices", args: []string{"."}, input: "1\n", failAt: 2, message: "write wizard choices"},
		{name: "choice prompt", args: []string{"."}, input: "1\n", failAt: 3, message: "read wizard choice"},
		{name: "open summary", args: []string{"."}, input: "1\n", failAt: 4, message: "write wizard open summary"},
		{name: "compile summary", args: []string{"."}, input: "2\n", failAt: 4, message: "write wizard compile summary"},
		{name: "help separator", args: []string{"."}, input: "3\n", failAt: 4, message: "write wizard help separator"},
		{name: "validation", args: []string{"."}, input: "invalid\n", failAt: 4, message: "write wizard validation"},
		{name: "cancellation", args: []string{"."}, input: "q\n", failAt: 4, message: "write wizard cancellation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := &wizardCalls{}
			writer := &wizardFailWriter{failAt: test.failAt}
			err := runWizardWith(test.args, strings.NewReader(test.input), writer, calls.actions())
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v after %d writes", err, writer.writes)
			}
			if len(calls.opened) != 0 || len(calls.compiled) != 0 || calls.helpCalls != 0 {
				t.Fatalf("output failure started action: %#v", calls)
			}
		})
	}

	if err := writeWizardCancellation(&wizardFailWriter{failAt: 1}, "Cancelled"); err == nil {
		t.Fatal("direct cancellation output failure succeeded")
	}
	if _, _, err := readWizardLine(bufio.NewScanner(strings.NewReader("value\n")), &wizardFailWriter{failAt: 1}, "prompt"); err == nil {
		t.Fatal("direct prompt output failure succeeded")
	}
}
