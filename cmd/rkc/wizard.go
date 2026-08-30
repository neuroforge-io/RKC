package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const wizardMaximumInputBytes = 64 * 1024

type wizardActions struct {
	Open       func([]string) error
	Quickstart func([]string) error
	Help       func(io.Writer) error
}

func runWizard(args []string) error {
	return runWizardWith(args, os.Stdin, os.Stdout, wizardActions{
		Open:       runOpen,
		Quickstart: runQuickstart,
		Help:       printUsageTo,
	})
}

// runWizardWith keeps the guided first run testable without starting a scan or
// server. The production actions call the existing open and quickstart
// services directly; no command string or shell is involved.
func runWizardWith(args []string, input io.Reader, output io.Writer, actions wizardActions) error {
	if input == nil || output == nil {
		return errors.New("wizard input and output are required")
	}
	if actions.Open == nil || actions.Quickstart == nil || actions.Help == nil {
		return errors.New("wizard actions are not configured")
	}

	fs := flag.NewFlagSet("wizard", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		_, _ = fmt.Fprint(output, `Usage:
  rkc wizard [folder]
  rkc tui [folder]

Guides you through the safe first run. It can build and open a read-only local
browser atlas, compile and verify an atlas without serving it, or show the full
command reference. The wizard is not a replacement for every CLI option.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("wizard accepts at most one folder path")
	}

	if _, err := fmt.Fprint(output, `RKC guided first run
This guide covers the safest common workflows. Use "rkc help" for every command.

`); err != nil {
		return fmt.Errorf("write wizard introduction: %w", err)
	}

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), wizardMaximumInputBytes)
	folder := ""
	if fs.NArg() == 1 {
		folder = fs.Arg(0)
	} else {
		value, ok, err := readWizardLine(scanner, output, "Folder to catalogue [.]: ")
		if err != nil {
			return fmt.Errorf("read wizard folder: %w", err)
		}
		if !ok {
			return writeWizardCancellation(output, "No input received")
		}
		folder = strings.TrimSpace(value)
		if folder == "" {
			folder = "."
		}
	}

	if _, err := fmt.Fprint(output, `
Choose what RKC should do:
  1. Build, verify, and open a local read-only browser atlas
  2. Build and verify the atlas without starting a browser or server
  3. Show the full command reference
  q. Cancel without starting work

`); err != nil {
		return fmt.Errorf("write wizard choices: %w", err)
	}

	for {
		choice, ok, err := readWizardLine(scanner, output, "Choice [1]: ")
		if err != nil {
			return fmt.Errorf("read wizard choice: %w", err)
		}
		if !ok {
			return writeWizardCancellation(output, "No choice received")
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "", "1", "o", "open":
			if _, err := fmt.Fprintf(output, "\nBuilding and opening a read-only atlas for %q. Press Ctrl-C to stop the local server.\n\n", folder); err != nil {
				return fmt.Errorf("write wizard open summary: %w", err)
			}
			// The separator keeps an arbitrary folder beginning with '-' from
			// being reinterpreted as an open option by the existing flag parser.
			if err := actions.Open([]string{"--", folder}); err != nil {
				return fmt.Errorf("wizard open: %w", err)
			}
			return nil
		case "2", "c", "compile", "quickstart":
			if _, err := fmt.Fprintf(output, "\nBuilding and verifying an atlas for %q without starting a server.\n\n", folder); err != nil {
				return fmt.Errorf("write wizard compile summary: %w", err)
			}
			if err := actions.Quickstart([]string{"--", folder}); err != nil {
				return fmt.Errorf("wizard compile: %w", err)
			}
			return nil
		case "3", "h", "help":
			if _, err := fmt.Fprint(output, "\n"); err != nil {
				return fmt.Errorf("write wizard help separator: %w", err)
			}
			if err := actions.Help(output); err != nil {
				return fmt.Errorf("write wizard help: %w", err)
			}
			return nil
		case "q", "quit", "cancel", "exit":
			return writeWizardCancellation(output, "Cancelled")
		default:
			if _, err := fmt.Fprintln(output, "Please enter 1, 2, 3, or q."); err != nil {
				return fmt.Errorf("write wizard validation: %w", err)
			}
		}
	}
}

func readWizardLine(scanner *bufio.Scanner, output io.Writer, prompt string) (string, bool, error) {
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return "", false, err
	}
	if scanner.Scan() {
		return scanner.Text(), true, nil
	}
	if err := scanner.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

func writeWizardCancellation(output io.Writer, reason string) error {
	if _, err := fmt.Fprintf(output, "\n%s; nothing was started.\n", reason); err != nil {
		return fmt.Errorf("write wizard cancellation: %w", err)
	}
	return nil
}
