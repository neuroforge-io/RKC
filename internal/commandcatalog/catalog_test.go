package commandcatalog

import (
	"reflect"
	"testing"
)

func TestCommandsAreUniqueValidAndIndependentlyOwned(t *testing.T) {
	commands := Commands(Context{})
	if len(commands) != 19 {
		t.Fatalf("command count = %d, want 19", len(commands))
	}
	seen := make(map[string]bool, len(commands))
	for _, command := range commands {
		if command.Name == "" || command.Description == "" || command.Guidance == "" {
			t.Fatalf("incomplete command: %+v", command)
		}
		if seen[command.Name] {
			t.Fatalf("duplicate command %q", command.Name)
		}
		seen[command.Name] = true
		switch command.Mode {
		case ModeRead, ModeWrites, ModeModel:
		default:
			t.Fatalf("invalid mode for %q: %q", command.Name, command.Mode)
		}
	}
	byName := make(map[string]Command, len(commands))
	for _, command := range commands {
		byName[command.Name] = command
	}
	for name, want := range map[string][]string{
		"plan":      {"."},
		"scan":      {"--no-python", "--out", ".rkc", "--state-dir", ".rkc-state", "."},
		"check":     {"--coverage", ".rkc/coverage.json"},
		"query":     {"--dir", ".rkc", "resource guard"},
		"snapshots": {"list", "--help"},
		"runs":      {"list", "--help"},
		"plugins":   {"list", "--help"},
		"cache":     {"inspect", "--help"},
	} {
		if got := byName[name].DefaultArgs; !reflect.DeepEqual(got, want) {
			t.Errorf("%s defaults = %#v, want %#v", name, got, want)
		}
	}
	commands[0].DefaultArgs[0] = "changed"
	if Commands(Context{})[0].DefaultArgs[0] != "." {
		t.Fatal("command defaults share mutable storage across calls")
	}
}

func TestCommandsBindExactDatasetAndCoverageSelections(t *testing.T) {
	context := Context{
		DatasetArgs: []string{"--database", "/tmp/catalogue.sqlite", "--snapshot", "snapshot-1"},
		CheckArgs:   []string{"--help"},
	}
	commands := Commands(context)
	byName := make(map[string]Command, len(commands))
	for _, command := range commands {
		byName[command.Name] = command
	}
	if got, want := byName["check"].DefaultArgs, []string{"--help"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("check defaults = %#v, want %#v", got, want)
	}
	if got, want := byName["query"].DefaultArgs, []string{"--database", "/tmp/catalogue.sqlite", "--snapshot", "snapshot-1", "resource guard"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("query defaults = %#v, want %#v", got, want)
	}
	context.DatasetArgs[1] = "changed"
	if byName["query"].DefaultArgs[1] != "/tmp/catalogue.sqlite" {
		t.Fatal("catalogue retained caller-owned selection storage")
	}
}
