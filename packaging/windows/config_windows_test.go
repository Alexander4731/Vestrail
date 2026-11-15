package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPortableConfigPreservesLegacyDatabase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	raw := []byte(`{"database":{"host":"127.0.0.1","port":5433,"user":"artex","password":"existing","dbname":"artex"},"skill_dir":"skills"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfiguration(root)
	if err != nil || cfg.Database.User != "artex" || cfg.Database.DBName != "artex" || cfg.Database.Password != "existing" {
		t.Fatalf("legacy database configuration was not preserved: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(raw) {
		t.Fatal("existing config.json was modified")
	}
}

func TestPortableNewInstallation(t *testing.T) {
	cfg, err := loadConfiguration(t.TempDir())
	if err != nil || cfg.Database.User != "vestrail" || cfg.Database.DBName != "vestrail" || len(cfg.Database.Password) < 32 {
		t.Fatalf("new installation configuration failed: %v", err)
	}
}
