package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("path", "rkc.json", "configuration path")
	force := fs.Bool("force", false, "replace an existing configuration")
	stdout := fs.Bool("stdout", false, "print the default configuration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("init does not accept positional arguments")
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
	if _, err := os.Stat(*path); err == nil && !*force {
		return fmt.Errorf("configuration already exists: %s; use --force", *path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*path, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", *path)
	return nil
}
