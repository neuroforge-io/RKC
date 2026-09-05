//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPathComparisonUsesPhysicalAncestors(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		parent, candidate string
		want              bool
	}{
		{root, root, true},
		{root, filepath.Join(child, "not-created", "yet"), true},
		{child, root, false},
		{child, filepath.Join(root, "sibling"), false},
		{filepath.Join(root, "absent"), filepath.Join(root, "absent", "child"), true},
	} {
		got, err := pathIsWithin(test.parent, test.candidate)
		if err != nil || got != test.want {
			t.Fatalf("pathIsWithin(%q, %q) = %v, %v; want %v", test.parent, test.candidate, got, err, test.want)
		}
	}
	file := filepath.Join(root, "regular-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pathIsWithin(root, filepath.Join(file, "child")); err == nil {
		t.Fatal("file ancestor was treated as an absent directory")
	}
	if _, err := windowsComparisonName(root + "\x00invalid"); err == nil {
		t.Fatal("invalid native path was accepted")
	}
	for _, component := range []string{"future.", "future ", "future:stream"} {
		if _, err := windowsComparisonName(filepath.Join(root, component, "child")); err == nil {
			t.Fatalf("ambiguous prospective component %q was accepted", component)
		}
	}
	t.Run("dangling-link", func(t *testing.T) {
		link := filepath.Join(root, "dangling-link")
		if err := os.Symlink(filepath.Join(root, "missing-target"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := windowsComparisonName(link); err == nil {
			t.Fatal("dangling link was treated as a prospective ordinary path")
		}
	})
}

func TestWindowsPathComparisonRetainsAliasedDriveContainment(t *testing.T) {
	root := t.TempDir()
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	letter := byte(0)
	for candidate := byte('Z'); candidate >= 'N'; candidate-- {
		if mask&(1<<uint(candidate-'A')) == 0 {
			letter = candidate
			break
		}
	}
	if letter == 0 {
		t.Fatal("no unused drive letter for the alias containment regression")
	}
	device, err := windows.UTF16PtrFromString(string(letter) + ":")
	if err != nil {
		t.Fatal(err)
	}
	target, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	// A temporary DOS-device mapping exercises the same alias as SUBST without
	// relying on an external command or replacing an existing drive mapping.
	if err := windows.DefineDosDevice(windows.DDD_NO_BROADCAST_SYSTEM, device, target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		flags := uint32(windows.DDD_NO_BROADCAST_SYSTEM | windows.DDD_REMOVE_DEFINITION | windows.DDD_EXACT_MATCH_ON_REMOVE)
		if err := windows.DefineDosDevice(flags, device, target); err != nil {
			t.Errorf("remove exact temporary drive mapping: %v", err)
		}
	})
	alias := string(letter) + `:\`
	if _, err := filepath.Rel(root, alias); err == nil {
		t.Fatal("alias fixture does not exercise different drive names")
	}
	for _, test := range []struct {
		parent, candidate string
	}{
		{root, alias},
		{alias, root},
		{root, filepath.Join(alias, "absent", "child")},
		{alias, filepath.Join(root, "absent", "child")},
	} {
		inside, err := pathIsWithin(test.parent, test.candidate)
		if err != nil || !inside {
			t.Fatalf("aliased drive lost containment: %q, %q = %v, %v", test.parent, test.candidate, inside, err)
		}
	}
	if inside, err := pathIsWithin(filepath.Join(root, "other"), alias); err != nil || inside {
		t.Fatalf("disjoint aliased path = %v, %v", inside, err)
	}
}
