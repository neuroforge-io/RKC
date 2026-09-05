package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/neuroforge-io/RKC/internal/discovery"
)

func runCapabilities(args []string) error {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	human := fs.Bool("human", false, "print a readable integration overview instead of JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("capabilities does not accept positional arguments")
	}
	value := discovery.Describe()
	if *human {
		fmt.Println("RKC connects people, programs, and agents to cited repository knowledge.")
		for _, output := range value.Outputs {
			fmt.Printf("\n%s (%s)\n  %s\n", output.Title, output.Format, output.Description)
		}
		fmt.Println("\nUse rkc capabilities for the JSON contract, or rkc <command> --help.")
		return nil
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
