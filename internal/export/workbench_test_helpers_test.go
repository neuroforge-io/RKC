package export

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The browser starts itself in production. Harnesses explicitly remove only
// that entry point; normal source-file whitespace must not trigger real boot.
func browserTestApplication(t testing.TB, application []byte) string {
	t.Helper()
	source := strings.TrimSpace(string(application))
	if !strings.HasSuffix(source, "boot();") {
		t.Fatal("browser application boot suffix is missing")
	}
	return strings.TrimSuffix(source, "boot();")
}

// A malformed mock or a regression in pagination must fail with a bounded
// subprocess, rather than leaving a hot Node process behind after a Go timeout.
func browserTestCommand(t testing.TB, executable string, arguments ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, executable, arguments...)
	command.WaitDelay = time.Second
	return command
}
