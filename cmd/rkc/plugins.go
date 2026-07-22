package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/neuroforge-io/RKC/internal/pluginregistry"
)

func runPlugins(args []string) error {
	if len(args) == 0 {
		return errors.New("plugins requires list, validate, lock, or verify")
	}
	switch args[0] {
	case "list":
		return runPluginsList(args[1:])
	case "validate":
		return runPluginsValidate(args[1:])
	case "lock":
		return runPluginsLock(args[1:])
	case "verify":
		return runPluginsVerify(args[1:])
	default:
		return fmt.Errorf("unknown plugins command %q", args[0])
	}
}
func discoverPlugins(root string) ([]pluginregistry.Manifest, []string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	return pluginregistry.Discover(absolute)
}
func pluginPolicy() pluginregistry.Policy {
	return pluginregistry.Policy{AllowedRuntimes: map[string]struct{}{"builtin": {}, "wasm-wasi": {}, "native-worker": {}}, AllowNetwork: false, AllowProcessSpawn: false, MaximumMemoryMiB: 4096, MaximumTimeout: 3600}
}

func runPluginsList(args []string) error {
	fs := flag.NewFlagSet("plugins list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "plugins", "plugin search root")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifests, paths, err := discoverPlugins(*root)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(map[string]any{"items": manifests, "paths": paths})
	}
	for i, m := range manifests {
		fmt.Printf("%-32s %-12s %-14s %s\n", m.Plugin.ID, m.Plugin.Version, m.Runtime.Kind, paths[i])
	}
	return nil
}
func runPluginsValidate(args []string) error {
	fs := flag.NewFlagSet("plugins validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "plugins", "plugin search root")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifests, paths, err := discoverPlugins(*root)
	if err != nil {
		return err
	}
	var failures []string
	for i, m := range manifests {
		if err := pluginregistry.Validate(m, pluginPolicy()); err != nil {
			failures = append(failures, paths[i]+": "+err.Error())
		}
	}
	result := map[string]any{"passed": len(failures) == 0, "plugins": len(manifests), "failures": failures}
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	fmt.Printf("Validated %d plugin manifest(s).\n", len(manifests))
	return nil
}
func runPluginsLock(args []string) error {
	fs := flag.NewFlagSet("plugins lock", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "plugins", "plugin search root")
	out := fs.String("out", "plugins/plugins.lock.json", "lockfile path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifests, _, err := discoverPlugins(*root)
	if err != nil {
		return err
	}
	lock := pluginregistry.BuildLock(manifests)
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("Locked %d plugin(s) in %s; digest %s\n", len(lock.Plugins), *out, pluginregistry.LockDigest(lock))
	return nil
}
func runPluginsVerify(args []string) error {
	fs := flag.NewFlagSet("plugins verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "plugins", "plugin search root")
	lockPath := fs.String("lock", "plugins/plugins.lock.json", "lockfile path")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifests, paths, err := discoverPlugins(*root)
	if err != nil {
		return err
	}
	lock, err := pluginregistry.LoadLock(*lockPath)
	if err != nil {
		return err
	}
	errs := pluginregistry.VerifyLock(*root, lock, manifests, paths)
	failures := make([]string, 0, len(errs))
	for _, item := range errs {
		failures = append(failures, item.Error())
	}
	result := map[string]any{"passed": len(failures) == 0, "plugins": len(manifests), "lock_digest": pluginregistry.LockDigest(lock), "failures": failures}
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	fmt.Printf("Verified %d locked plugin(s); digest %s\n", len(manifests), pluginregistry.LockDigest(lock))
	return nil
}
