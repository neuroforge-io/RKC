package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/privatepath"
)

func runProviders(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runProvidersWithIO(ctx, args, os.Stdout, os.Stderr)
}

func runProvidersWithIO(ctx context.Context, args []string, output, diagnostics io.Writer) error {
	if ctx == nil || output == nil || diagnostics == nil {
		return errors.New("provider command context and output streams are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(output, `Usage: rkc providers <command> [options]

  list         Show local, hosted API, and subscription-client connection paths
  init         Print or save a portable connection profile; choose a model explicitly
  doctor       Check a profile and credential presence locally, without a request
  models       Explicitly fetch one model-metadata page; never generate text
  login-guide  Show sign-in steps for installed Codex, Claude Code, or Gemini CLI

Start with: rkc providers list
Then: rkc providers init --preset ollama --model YOUR_MODEL --out provider.json
Remote API profiles require explicit --allow-remote when created.
Use a profile: rkc answer --provider-config provider.json --dir ATLAS "question"
Subscription sign-in is handled by its supported client; RKC never reads its tokens.`)
		return err
	}
	fs := flag.NewFlagSet("providers "+args[0], flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	switch args[0] {
	case "list":
		jsonOutput := fs.Bool("json", false, "print the connection template catalog as JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("providers list does not accept positional arguments")
		}
		if *jsonOutput {
			return writeProviderJSON(output, modelruntime.ProviderPresets())
		}
		for _, preset := range modelruntime.ProviderPresets() {
			location := "local"
			if preset.Remote {
				location = "remote HTTPS"
			}
			if _, err := fmt.Fprintf(output, "%s (%s) — %s\n  %s\n", preset.ID, location, preset.Name, preset.Description); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintln(output, "\nAlready have a ChatGPT, Claude, or Google account? Use `rkc providers login-guide` for the supported client sign-in and cited context workflow. No model or credentials are selected automatically.")
		return err
	case "init":
		preset := fs.String("preset", "", "connection preset from providers list")
		model := fs.String("model", "", "explicit model ID available on your API or local server")
		endpoint := fs.String("endpoint", "", "override with the exact generation URL")
		credential := fs.String("api-key-env", "", "credential environment variable name; never its value")
		remote := fs.Bool("allow-remote", false, "allow sending selected evidence to this remote HTTPS API")
		out := fs.String("out", "", "new profile file; existing files are never overwritten; default prints JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("providers init does not accept positional arguments")
		}
		profile, err := modelruntime.NewProviderProfile(*preset, *model, *remote)
		if err != nil {
			return err
		}
		if *endpoint != "" {
			profile.Endpoint = *endpoint
		}
		if flagWasSet(fs, "api-key-env") {
			profile.APIKeyEnv = *credential
		}
		if err := profile.Validate(); err != nil {
			return err
		}
		data, err := modelruntime.MarshalProviderProfile(profile)
		if err != nil {
			return err
		}
		if *out == "" {
			_, err = output.Write(data)
			return err
		}
		if err := publishProviderProfile(ctx, *out, data); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "Saved %s. Check it with: rkc providers doctor --file %s\n", *out, *out)
		return err
	case "doctor", "models":
		file := fs.String("file", "", "portable provider profile JSON")
		jsonOutput := fs.Bool("json", false, "print a machine-readable report")
		preset := fs.String("preset", "", "models only: choose a connection preset without selecting a generation model")
		endpoint := fs.String("endpoint", "", "models only: override the exact generation URL used to derive the metadata route")
		credential := fs.String("api-key-env", "", "models only: credential environment variable name")
		remote := fs.Bool("allow-remote", false, "models only: consent to the metadata GET on a remote HTTPS API")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("providers doctor/models do not accept positional arguments")
		}
		var profile modelruntime.ProviderProfile
		var err error
		if args[0] == "models" && *file == "" && *preset != "" {
			profile, err = modelruntime.NewProviderProfile(*preset, "catalog-only", *remote)
			if *endpoint != "" {
				profile.Endpoint = *endpoint
			}
			if flagWasSet(fs, "api-key-env") {
				profile.APIKeyEnv = *credential
			}
			if err == nil {
				err = profile.Validate()
			}
		} else {
			if *preset != "" || *endpoint != "" || flagWasSet(fs, "api-key-env") || flagWasSet(fs, "allow-remote") {
				return errors.New("preset/endpoint/api-key-env/allow-remote require providers models without --file")
			}
			profile, err = loadProviderProfile(*file)
		}
		if err != nil {
			return err
		}
		if args[0] == "doctor" {
			credentialErr := modelruntime.CheckCredential(profile.APIKeyEnv)
			checks := []string{"Connection policy is valid.", "No network request was made.", "Model access, quality, schema support and price have not been checked."}
			if credentialErr == nil {
				checks = append(checks, "Selected credential is present or this connection is anonymous.")
			} else {
				checks = append(checks, credentialErr.Error())
			}
			if *jsonOutput {
				if err := writeProviderJSON(output, struct {
					Ready          bool                         `json:"ready"`
					NetworkChecked bool                         `json:"network_checked"`
					Connection     modelruntime.ProviderProfile `json:"connection"`
					Checks         []string                     `json:"checks"`
				}{credentialErr == nil, false, profile, checks}); err != nil {
					return err
				}
			} else {
				for _, check := range checks {
					if _, err := fmt.Fprintln(output, check); err != nil {
						return err
					}
				}
			}
			return credentialErr
		}
		provider, err := modelruntime.NewAPIProvider(profile, qualifiedClaimResponseSchema)
		if err != nil {
			return err
		}
		defer provider.Close()
		catalog, err := provider.DiscoverModels(ctx)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeProviderJSON(output, catalog)
		}
		for _, model := range catalog.Models {
			if _, err := fmt.Fprintln(output, model.ID); err != nil {
				return err
			}
		}
		if catalog.MoreAvailable {
			if _, err := fmt.Fprintln(output, "More models are available: this is one bounded metadata page."); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(output, "Metadata only: generation, schema compatibility, account entitlement and price were not tested.")
		return err
	case "login-guide":
		client := fs.String("client", "", "codex, claude, or gemini; default shows all")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("providers login-guide does not accept positional arguments")
		}
		guides := []struct{ id, name, steps, docs string }{
			{"codex", "ChatGPT with Codex", "Run codex login, then codex login status. Sign-in and plan access are controlled by Codex.", "https://learn.chatgpt.com/docs/auth"},
			{"claude", "Claude Code", "Run claude and follow its sign-in flow. Use /status in Claude Code to verify the selected account and authentication method.", "https://code.claude.com/docs/en/authentication"},
			{"gemini", "Gemini CLI", "Run gemini and select Sign in with Google. Account access is controlled by Gemini CLI.", "https://geminicli.com/docs/get-started/authentication/"},
		}
		found := false
		for _, guide := range guides {
			if *client != "" && *client != guide.id {
				continue
			}
			found = true
			installed := "not found on PATH"
			if _, err := exec.LookPath(guide.id); err == nil {
				installed = "installed; login state was not inspected"
			}
			if _, err := fmt.Fprintf(output, "%s (%s)\n%s\n%s\n\n", guide.name, installed, guide.steps, guide.docs); err != nil {
				return err
			}
		}
		if !found {
			return errors.New("client must be codex, claude, or gemini")
		}
		_, err := fmt.Fprintln(output, "Export selected evidence: rkc context --dir ATLAS --format markdown \"your question\"\nReview the export, then share it with your signed-in client, or configure its RKC MCP integration. RKC does not automate login, read client tokens, or convert subscriptions into API keys. Direct ChatGPT plan sign-in is a separate preview integration and is not implemented in RKC.")
		return err
	default:
		return fmt.Errorf("unknown providers command %q; use rkc providers help", args[0])
	}
}

// publishProviderProfile makes only a fully written, synced private inode
// visible. Hard-link publication is atomic and no-clobber; filesystems lacking
// this primitive fail explicitly rather than publishing a partial fallback.
func publishProviderProfile(ctx context.Context, path string, data []byte) error {
	if ctx == nil {
		return errors.New("provider profile publication context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve provider profile path: %w", err)
	}
	parent := filepath.Dir(absolute)
	directoryIdentity, err := privatepath.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect provider profile directory: %w", err)
	}
	if !directoryIdentity.IsDir() || directoryIdentity.Mode()&os.ModeSymlink != 0 {
		return errors.New("provider profile parent must be a real existing directory")
	}
	temporary, err := privatepath.CreateTemp(parent, "."+filepath.Base(absolute)+".tmp-")
	if err != nil {
		return fmt.Errorf("create private provider profile staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	defer temporary.Close()
	identity, err := temporary.Stat()
	if err != nil {
		return fmt.Errorf("inspect provider profile staging file: %w", err)
	}
	if err := privatepath.CheckFile(temporaryPath, identity); err != nil {
		return fmt.Errorf("protect provider profile staging file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write provider profile staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync provider profile staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close provider profile staging file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := privatepath.SyncDirectoryStable(parent, directoryIdentity); err != nil {
		return fmt.Errorf("verify provider profile publication directory: %w", err)
	}
	if err := os.Link(temporaryPath, absolute); err != nil {
		return fmt.Errorf("publish complete provider profile without replacement (hard-link support required): %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("profile published completely but staging cleanup failed: %w", err)
	}
	if err := privatepath.SyncDirectoryStable(parent, directoryIdentity); err != nil {
		return fmt.Errorf("profile published completely but directory durability could not be confirmed: %w", err)
	}
	return nil
}

func loadProviderProfile(path string) (modelruntime.ProviderProfile, error) {
	if strings.TrimSpace(path) == "" {
		return modelruntime.ProviderProfile{}, errors.New("provider profile file is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return modelruntime.ProviderProfile{}, fmt.Errorf("read provider profile: %w", err)
	}
	defer file.Close()
	return modelruntime.ReadProviderProfile(file)
}

func writeProviderJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
