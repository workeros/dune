// Package storage configures Dune's supported metadata backends. Applications
// select one backend; domain stores and transactions are created together by
// Dune and cannot be replaced independently.
package storage

import (
	"context"
	"github.com/jackc/pgx/v5"
)

type Config struct {
	// SQLiteDir is an absolute private directory. Only one Dune instance may
	// hold it. Exactly one of SQLiteDir and Postgres must be configured.
	SQLiteDir string
	Postgres  *Postgres
}

type Postgres struct {
	URL string
	// BeforeConnect receives a private copy of the connection configuration for
	// every new physical connection, including replacements in the pool. It may
	// refresh credentials or configure connection establishment through a private
	// SDK. It must honor context and be safe for concurrent calls. Existing live
	// connections are not reauthenticated. The pool is owned and closed by Dune.
	// Every connection must address the same database and schema namespace.
	BeforeConnect func(context.Context, *pgx.ConnConfig) error
}
