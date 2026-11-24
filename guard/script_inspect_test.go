package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnrichBashDescriptionWithScripts(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "i1292")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sub, "p_secondary.py")
	scriptBody := "tests = [('POST', 'api/cluster/restoreFactorySettings', '{}')]\n"
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o600); err != nil {
		t.Fatal(err)
	}
	// Outside file must never be read via traversal.
	outside := filepath.Join(filepath.Dir(dir), "secret_outside.py")
	_ = os.WriteFile(outside, []byte("OUTSIDE_SECRET"), 0o600)
	t.Cleanup(func() { _ = os.Remove(outside) })

	cmdJSON, _ := json.Marshal(map[string]string{
		"command": "cd i1292; python p_secondary.py 2>&1",
	})
	enriched := enrichBashDescriptionWithScripts(dir, cmdJSON)
	if !strings.Contains(string(enriched), "restoreFactorySettings") {
		t.Fatalf("expected referenced script body in enriched description, got %s", string(enriched))
	}

	traversalJSON, _ := json.Marshal(map[string]string{
		"command": "python ../secret_outside.py",
	})
	travEnriched := enrichBashDescriptionWithScripts(dir, traversalJSON)
	if strings.Contains(string(travEnriched), "OUTSIDE_SECRET") {
		t.Fatalf("traversal script outside workingDir must never be read: %s", string(travEnriched))
	}
}
