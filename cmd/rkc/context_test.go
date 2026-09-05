package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcapi"
)

func TestContextAndCapabilitiesCommands(t *testing.T) {
	output, err := captureStdout(t, func() error { return run([]string{"capabilities"}) })
	var capabilities rkcapi.Capabilities
	if err != nil || json.Unmarshal([]byte(output), &capabilities) != nil || capabilities.SchemaVersion != "rkc-capabilities/v1" {
		t.Fatalf("capabilities: %s %v", output, err)
	}
	output, err = captureStdout(t, func() error { return run([]string{"capabilities", "--human"}) })
	if err != nil || !strings.Contains(output, "Knowledge pack") {
		t.Fatalf("human discovery: %s %v", output, err)
	}
	for _, args := range [][]string{{"capabilities", "extra"}, {"capabilities", "--unknown"}, {"context"}, {"context", "--format", "html", "query"}, {"context", "--unknown"}} {
		if err := run(args); err == nil {
			t.Errorf("accepted invalid command %v", args)
		}
	}
	atlas := flowScanFixture(t)
	output, err = captureStdout(t, func() error { return run([]string{"context", "--dir", atlas, "--limit", "2", "load"}) })
	var packet rkcapi.ContextPacket
	if err != nil || json.Unmarshal([]byte(output), &packet) != nil || len(packet.Items) == 0 || packet.Digest == "" {
		t.Fatalf("context: %s %v", output, err)
	}
	output, err = captureStdout(t, func() error { return run([]string{"context", "--dir", atlas, "--format", "markdown", "load"}) })
	if err != nil || !strings.Contains(output, "# RKC cited context") {
		t.Fatalf("markdown context: %s %v", output, err)
	}
	if err := run([]string{"context", "--dir", atlas, "--limit", "51", "load"}); err == nil {
		t.Fatal("accepted bad retrieval limit")
	}
	if err := run([]string{"context", "--dir", t.TempDir(), "load"}); err == nil {
		t.Fatal("accepted invalid dataset")
	}
}
