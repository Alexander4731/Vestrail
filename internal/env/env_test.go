package env

import (
	"os"
	"testing"
)

func TestLegacyPrecedence(t *testing.T) {
	const name = "VESTRAIL_TEST_SETTING"
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_TEST_SETTING", "legacy")
	if value, source := Lookup(name); value != "legacy" || source != "ARTEX_TEST_SETTING" {
		t.Fatalf("legacy lookup: %q, %q", value, source)
	}
	t.Setenv(name, "current")
	if value, source := Lookup(name); value != "current" || source != name {
		t.Fatalf("current lookup: %q, %q", value, source)
	}
	t.Setenv(name, "")
	if Get(name) != "" {
		t.Fatal("an explicit empty value must suppress legacy credentials and flags")
	}
}
