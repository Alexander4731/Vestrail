package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyConfigAndDatabasePreserved(t *testing.T) {
	for _, name := range []string{"VESTRAIL_CONFIG", "VESTRAIL_PG_DSN"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"database":{"host":"127.0.0.1","user":"artex","dbname":"artex","port":5433},"skill_dir":"existing-skills"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_CONFIG", path)
	t.Setenv("ARTEX_PG_DSN", "")
	if Path() != path || Load().SkillDir != "existing-skills" {
		t.Fatal("legacy configuration was not read")
	}
	if dsn, _, err := PostgresDSN(); err != nil || dsn != "postgres://artex@127.0.0.1:5433/artex?sslmode=disable" {
		t.Fatalf("legacy database changed: %q %v", dsn, err)
	}
	t.Setenv("ARTEX_PG_DSN", "postgres://legacy/old")
	if dsn, source, err := PostgresDSN(); err != nil || dsn != "postgres://legacy/old" || source != "环境变量 ARTEX_PG_DSN" {
		t.Fatalf("legacy DSN: %q %q %v", dsn, source, err)
	}
	t.Setenv("VESTRAIL_PG_DSN", "postgres://current/new")
	if dsn, _, err := PostgresDSN(); err != nil || dsn != "postgres://current/new" {
		t.Fatalf("new DSN precedence: %q %v", dsn, err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(raw) {
		t.Fatal("loading legacy configuration must not rewrite it")
	}
}
