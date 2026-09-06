package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateDatabaseConfiguration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "database.yaml")
	for _, source := range []string{"sqlite_dir: /private/dune\n", "postgres:\n  url: postgres://user:private-password@localhost/dune\n"} {
		if err := os.WriteFile(file, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		value, err := Database(file)
		if err != nil {
			t.Fatal(err)
		}
		if value.SQLiteDir == "" && value.Postgres == nil {
			t.Fatal("database selection lost")
		}
	}
	for _, source := range []string{
		"postgres:\n  url: [private-password]\n",
		"postgres:\n  url: postgres://localhost/dune\n  unknown: private-password\n",
		"sqlite_dir: /private/dune\n---\nsecret: private-password\n",
		"sqlite_dir: /private/dune\npostgres:\n  url: postgres://localhost/dune\n",
		"{}",
	} {
		if err := os.WriteFile(file, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Database(file); err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatal("invalid configuration accepted or disclosed its secret", err)
		}
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Database(file); err == nil {
		t.Fatal("shared database configuration accepted")
	}
}
