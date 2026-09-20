package sessionregistry

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/internal/launchgate"
	"modernc.org/sqlite"
)

// OpenReadOnly never creates a directory, database, schema or admission record.
// A missing database is an error, allowing callers to distinguish first install
// from an unreadable/older registry. SQLite enforces read-only access as well.
func OpenReadOnly(ctx context.Context, directory string) (*Registry, error) {
	if err := launchgate.CheckDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "registry.sqlite")
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		f, err := os.OpenFile(path+suffix, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if suffix != "" && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return nil, err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink > 1 || (suffix == "" && owner.Nlink != 1) {
			return nil, fmt.Errorf("invalid private registry file")
		}
	}
	dsn := &url.URL{Scheme: "file", Path: path}
	query := dsn.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(ON)")
	query.Add("_pragma", "busy_timeout(1000)")
	dsn.RawQuery = query.Encode()
	connector, err := sqlite.NewConnector(dsn.String())
	if err != nil {
		return nil, err
	}
	r := &Registry{db: sql.OpenDB(connector)}
	r.db.SetMaxOpenConns(1)
	if err := r.db.QueryRowContext(ctx, `SELECT max_keys,max_controls FROM registry_settings WHERE id=1`).Scan(&r.maxKeys, &r.maxControls); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
