package config

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/aiomni/dune/pkg/storage"
	"gopkg.in/yaml.v3"
)

// Database reads the official application's private database configuration.
// Go hosts use storage.Config directly for connection-authentication callbacks.
func Database(path string) (storage.Config, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return storage.Config{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return storage.Config{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() || info.Size() > 64*1024 {
		return storage.Config{}, fmt.Errorf("database config must be a private, owned regular file of at most 64 KiB")
	}
	var value struct {
		SQLiteDir string `yaml:"sqlite_dir"`
		Postgres  *struct {
			URL string `yaml:"url"`
		} `yaml:"postgres"`
	}
	decoder := yaml.NewDecoder(io.LimitReader(f, 64*1024))
	decoder.KnownFields(true)
	if err := decoder.Decode(&value); err != nil {
		return storage.Config{}, fmt.Errorf("invalid database configuration")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return storage.Config{}, fmt.Errorf("one database configuration required")
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
