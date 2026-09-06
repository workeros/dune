package config

import (
	"fmt"

	"github.com/aiomni/dune/pkg/storage"
)

// Database reads the official application's private database configuration.
// Go hosts use storage.Config directly for connection-authentication callbacks.
func Database(path string) (storage.Config, error) {
	var value struct {
		SQLiteDir string `yaml:"sqlite_dir"`
		Postgres  *struct {
			URL string `yaml:"url"`
		} `yaml:"postgres"`
	}
	if err := privateYAML(path, "database", &value); err != nil {
		return storage.Config{}, err
	}
	if (value.SQLiteDir == "") == (value.Postgres == nil) {
		return storage.Config{}, fmt.Errorf("choose exactly one SQLite or PostgreSQL backend")
	}
	result := storage.Config{SQLiteDir: value.SQLiteDir}
	if value.Postgres != nil {
		result.Postgres = &storage.Postgres{URL: value.Postgres.URL}
	}
	return result, nil
}
