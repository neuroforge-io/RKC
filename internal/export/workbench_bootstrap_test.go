package export

import (
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserAssetsConsumeWorkbenchBootstrapWithoutPersistentURLSecrets(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "bootstrap.go", []byte("package bootstrap\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"takeWorkbenchBootstrap",
		"new URLSearchParams(fragment)",
		"values.get('rkc-workbench')",
		"values.delete('rkc-workbench')",
		"history.replaceState",
		"sessionStorage.getItem('rkc-workbench-token')",
		"sessionStorage.setItem('rkc-workbench-token',token)",
		"sessionStorage.removeItem('rkc-workbench-token')",
		"authority_notice",
		"not a security sandbox",
		"headers['X-RKC-Workbench-Bootstrap']=bootstrap",
		"headers['X-RKC-Workbench-Token']=stored",
		"fetch('/api/v1/workbench/session',{cache:'no-store',headers})",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser application is missing protected-bootstrap marker %q", marker)
		}
	}
	removeFragment := strings.Index(application, "history.replaceState")
	exchangeSession := strings.Index(application, "fetch('/api/v1/workbench/session'")
	if removeFragment < 0 || exchangeSession < 0 || removeFragment >= exchangeSession {
		t.Fatalf("bootstrap URL cleanup must precede session exchange: cleanup=%d exchange=%d", removeFragment, exchangeSession)
	}
	if strings.Contains(application, "localStorage") ||
		strings.Contains(application, "sessionStorage.setItem('rkc-workbench-bootstrap'") ||
		strings.Contains(application, "?rkc-workbench=") {
		t.Fatal("browser application persists or transmits the bootstrap capability through an unsafe channel")
	}
}
