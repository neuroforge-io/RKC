package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var linkInitConfiguration = os.Link

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("path", "", "configuration path (default rkc.json)")
	out := fs.String("out", "", "compatibility alias for --path")
	force := fs.Bool("force", false, "replace an existing configuration")
	stdout := fs.Bool("stdout", false, "print the default configuration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("init does not accept positional arguments")
	}
	destination, err := resolveInitPath(*path, *out)
	if err != nil {
		return err
	}
	if *stdout && (*path != "" || *out != "" || *force) {
		return errors.New("--stdout cannot be combined with --path, --out, or --force")
	}
	cfg := defaultConfiguration()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *stdout {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := writeInitConfiguration(destination, data, *force); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", destination)
	fmt.Printf("Next: review the file, then run rkc doctor --config %s --repository .\n", destination)
	return nil
}

func writeInitConfiguration(path string, data []byte, force bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("configuration path cannot be empty")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create configuration parent: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration path must not be a symbolic link: %s", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("configuration path is not a regular file: %s", path)
		}
		if !force {
			return fmt.Errorf("configuration already exists: %s; use --force", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect configuration path: %w", err)
	}

	temporary, err := os.CreateTemp(parent, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create configuration staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return fmt.Errorf("set configuration permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write configuration staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync configuration staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close configuration staging file: %w", err)
	}
	if force {
		if err := os.Rename(temporaryPath, path); err != nil {
			return fmt.Errorf("replace configuration: %w", err)
		}
		return nil
	}
	// Publish the fully synced inode with no replacement window. A concurrent
	// creator wins and its file remains untouched.
	if err := linkInitConfiguration(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("configuration already exists: %s; use --force", path)
		}
		// Some otherwise valid portable filesystems do not implement hard links.
		// Preserve no-clobber semantics with an exclusive-create fallback. The
		// already-synced staging inode remains the preferred atomic path.
		return writeInitConfigurationExclusive(path, data, err)
	}
	return nil
}

func writeInitConfigurationExclusive(path string, data []byte, linkErr error) error {
	destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("configuration already exists: %s; use --force", path)
		}
		return fmt.Errorf("publish configuration without replacement (hard link unavailable: %v): %w", linkErr, err)
	}
	if _, err := destination.Write(data); err != nil {
		_ = destination.Close()
		return fmt.Errorf("write exclusively created configuration %s: %w; remove the incomplete file before retrying", path, err)
	}
	if err := destination.Sync(); err != nil {
		_ = destination.Close()
		return fmt.Errorf("sync exclusively created configuration %s: %w; remove the incomplete file before retrying", path, err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close exclusively created configuration %s: %w", path, err)
	}
	return nil
}

func resolveInitPath(path, out string) (string, error) {
	path = strings.TrimSpace(path)
	out = strings.TrimSpace(out)
	if path != "" && out != "" && filepath.Clean(path) != filepath.Clean(out) {
		return "", errors.New("--path and --out name different files; use only --path")
	}
	if path != "" {
		return path, nil
	}
	if out != "" {
		return out, nil
	}
	return "rkc.json", nil
}
