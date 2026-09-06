package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Transfer tables follow foreign-key order. Keep every persisted domain column
// here when advancing the schema; TestTransferSchemaCoverage prevents omissions.
var transferTables = []struct{ name, columns string }{
	{"dune_principals", "id,email,enabled,auth_version"},
	{"dune_external_identities", "namespace,subject,principal_id"},
	{"dune_identity_links", "request_id,actor,principal_id,namespace,subject,reason,created_at"},
	{"dune_discovery_cursors", "id,principal_id,identity_namespace,operation,after_id,expires_at"},
	{"dune_local_accounts", "principal_id,email,salt,password_hash"},
	{"dune_sessions", "hash,principal_id,expires_at,auth_version,identity_namespace,kind,parent_hash"},
	{"dune_cli_logins", "id,challenge,site,identity_namespace,expires_at,session_hash"},
	{"dune_login_transactions", "state_hash,browser_hash,namespace,redirect_url,nonce,verifier,expires_at"},
	{"dune_enrollments", "hash,principal_id,name,expires_at"},
	{"dune_runners", "id,owner_id,name,kind,fabric_id,binding_revision,created_at"},
	{"dune_machines", "id,runner_id,credential_hash,os,arch"},
	{"dune_access_tickets", "hash,session_hash,principal_id,identity_namespace,machine_id,runner_id,fabric_id,binding_revision,auth_version,expires_at,owner_id"},
}

func (s *Store) emptyImportTarget(ctx context.Context, tx *sql.Tx) error {
	if s.postgres {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1146441285)`); err != nil {
			return err
		}
		names := make([]string, len(transferTables))
		for i, table := range transferTables {
			names[i] = table.name
		}
		if _, err := tx.ExecContext(ctx, "LOCK TABLE "+strings.Join(names, ",")+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return err
		}
	}
	for _, table := range transferTables {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("import requires an empty SQL target; existing metadata was not changed")
		}
	}
	return nil
}

// CopySQLiteTo copies a locked, offline SQLite snapshot into an empty SQL target.
// There is no dual writing or external side effect. The source is read within a
// single transaction; all target records commit together and are never replayed.
func (s *Store) CopySQLiteTo(ctx context.Context, target *Store) (map[string]int64, error) {
	if s.postgres || s == target {
		return nil, fmt.Errorf("copy requires a separate SQLite source")
	}
	snapshot, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer snapshot.Rollback()
	counts := make(map[string]int64, len(transferTables))
	err = target.transaction(ctx, func(tx *sql.Tx) error {
		if err := target.emptyImportTarget(ctx, tx); err != nil {
			return err
		}
		for _, table := range transferTables {
			n, err := copyTable(ctx, snapshot, tx, table.name, table.columns)
			if err != nil {
				return err
			}
			counts[table.name] = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

func copyTable(ctx context.Context, source, target *sql.Tx, name, columns string) (int64, error) {
	query := "SELECT " + columns + " FROM " + name
	if name == "dune_sessions" {
		query += " ORDER BY CASE WHEN parent_hash IS NULL THEN 0 ELSE 1 END"
	}
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return 0, err
	}
	n := len(strings.Split(columns, ","))
	values, pointers, parameters := make([]any, n), make([]any, n), make([]string, n)
	for i := range n {
		pointers[i] = &values[i]
		parameters[i] = fmt.Sprintf("$%d", i+1)
	}
	insert, err := target.PrepareContext(ctx, "INSERT INTO "+name+"("+columns+") VALUES("+strings.Join(parameters, ",")+")")
	if err != nil {
		return 0, err
	}
	defer insert.Close()
	var count int64
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return 0, err
		}
		// SQLite stores BOOLEAN as integers; pgx requires a Go bool for the
		// PostgreSQL BOOLEAN parameter. Preserve the declared logical type.
		for i, value := range values {
			if strings.EqualFold(types[i].DatabaseTypeName(), "BOOLEAN") {
				if number, ok := value.(int64); ok && (number == 0 || number == 1) {
					values[i] = number == 1
				} else if _, ok := value.(bool); !ok {
					return 0, fmt.Errorf("invalid boolean in SQLite metadata")
				}
			}
		}
		if _, err := insert.ExecContext(ctx, values...); err != nil {
			return 0, err
		}
		count++
	}
	return count, rows.Err()
}
