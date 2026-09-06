// Package metadata implements the single transaction domain shared by Dune's
// product modules. SQL and driver details never enter the protocol cores.
package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"
)

var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrConflict        = errors.New("metadata conflict")
	ErrNotFound        = errors.New("not found")
	ErrCommitUnknown   = errors.New("metadata commit outcome unknown; do not automatically replay")
)

type Store struct {
	db       *sql.DB
	postgres bool
	lock     *os.File
	once     sync.Once
	closeErr error
}

func Open(ctx context.Context, config storage.Config) (*Store, error) {
	return open(ctx, config, true)
}

// OpenPostgresSource opens an existing current schema without initialization or
// migration. Offline recovery inspection must not silently upgrade a database.
func OpenPostgresSource(ctx context.Context, config *storage.Postgres) (*Store, error) {
	if config == nil {
		return nil, fmt.Errorf("PostgreSQL configuration required")
	}
	return open(ctx, storage.Config{Postgres: config}, false)
}

// OpenSQLiteSource takes the normal exclusive lock but never initializes or
// upgrades a source database. A typo or unrelated SQLite file must fail closed.
func OpenSQLiteSource(ctx context.Context, dir string) (*Store, error) {
	if _, err := os.Lstat(dir); err != nil {
		return nil, err
	}
	return open(ctx, storage.Config{SQLiteDir: dir}, false)
}

func open(ctx context.Context, config storage.Config, initialize bool) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if (config.SQLiteDir == "") == (config.Postgres == nil) {
		return nil, fmt.Errorf("choose exactly one SQLite or PostgreSQL backend")
	}
	s := &Store{postgres: config.Postgres != nil}
	var err error
	if s.postgres {
		options := config.Postgres
		if options.URL == "" {
			return nil, fmt.Errorf("PostgreSQL URL required")
		}
		connConfig, parseErr := pgx.ParseConfig(options.URL)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid PostgreSQL connection configuration")
		}
		before := options.BeforeConnect
		s.db = stdlib.OpenDB(*connConfig, stdlib.OptionBeforeConnect(func(ctx context.Context, c *pgx.ConnConfig) error {
			if before == nil {
				return nil
			}
			private := c.Copy()
			if err := before(ctx, private); err != nil {
				return err
			}
			*c = *private
			return nil
		}))
	} else {
		s.lock, err = lockDirectory(config.SQLiteDir)
		if err != nil {
			return nil, err
		}
		file := filepath.Join(config.SQLiteDir, "metadata.sqlite")
		if _, legacyErr := os.Lstat(filepath.Join(config.SQLiteDir, "accounts.json")); legacyErr == nil {
			info, dbErr := os.Lstat(file)
			if os.IsNotExist(dbErr) || (dbErr == nil && info.Size() == 0) {
				s.Close()
				return nil, fmt.Errorf("legacy JSON metadata requires explicit import into a new SQL directory")
			}
		}
		flags := os.O_RDWR | syscall.O_NOFOLLOW
		if initialize {
			flags |= os.O_CREATE
		}
		f, err := os.OpenFile(file, flags, 0600)
		if err == nil {
			info, statErr := f.Stat()
			if statErr != nil {
				err = statErr
			} else if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				err = fmt.Errorf("SQLite file must be private, owned by this user, regular and not hard-linked")
			} else if !initialize && info.Size() == 0 {
				err = fmt.Errorf("an existing SQLite metadata source is required")
			}
			f.Close()
		}
		if err != nil {
			s.Close()
			return nil, err
		}
		dsn := &url.URL{Scheme: "file", Path: file}
		query := dsn.Query()
		query.Set("_txlock", "immediate")
		query.Add("_pragma", "foreign_keys(1)")
		query.Add("_pragma", "busy_timeout(5000)")
		query.Add("_pragma", "synchronous(FULL)")
		dsn.RawQuery = query.Encode()
		connector, err := sqlite.NewConnector(dsn.String())
		if err != nil {
			s.Close()
			return nil, err
		}
		s.db = sql.OpenDB(connector)
	}
	s.db.SetMaxOpenConns(8)
	s.db.SetMaxIdleConns(4)
	s.db.SetConnMaxLifetime(30 * time.Minute)
	if err = s.db.PingContext(ctx); err == nil && !s.postgres && initialize {
		_, err = s.db.ExecContext(ctx, "PRAGMA journal_mode=WAL")
	}
	if err == nil {
		if initialize {
			err = s.migrate(ctx)
		} else {
			var version int
			err = s.db.QueryRowContext(ctx, `SELECT version FROM dune_schema WHERE id=1`).Scan(&version)
			if err == nil && version != schemaVersion {
				err = fmt.Errorf("unsupported metadata schema version %d", version)
			}
		}
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func lockDirectory(dir string) (*os.File, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
		return nil, fmt.Errorf("metadata directory must be private and absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("metadata directory must be owned by this user and mode 0700, not a symlink")
	}
	// Reuse the legacy lock name so an older JSON process cannot write into
	// this directory while its SQL replacement is running.
	lock, err := os.OpenFile(filepath.Join(dir, "accounts.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("metadata directory is already open: %w", err)
	}
	return lock, nil
}

func (s *Store) Close() error {
	s.once.Do(func() {
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
		if s.lock != nil {
			syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
			s.closeErr = errors.Join(s.closeErr, s.lock.Close())
		}
	})
	return s.closeErr
}

func conflict(err error) error {
	var pg *pgconn.PgError
	var sq *sqlite.Error
	if errors.As(err, &pg) && (pg.Code == "23505" || pg.Code == "23503" || pg.Code == "23514" || pg.Code == "40001" || pg.Code == "40P01") {
		return ErrConflict
	}
	if errors.As(err, &sq) && (sq.Code()&255 == 19 || sq.Code()&255 == 5) {
		return ErrConflict
	}
	return err
}

func (s *Store) transaction(ctx context.Context, change func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return conflict(err)
	}
	defer tx.Rollback()
	if err := change(tx); err != nil {
		return conflict(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %v", ErrCommitUnknown, err)
	}
	return nil
}
