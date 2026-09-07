package host_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/storage"
)

func TestClusterRejectsInvalidConfigurationBeforeStorage(t *testing.T) {
	for _, database := range []*storage.Config{nil, {Postgres: &storage.Postgres{URL: "invalid"}}} {
		options := host.Options{PublicURL: "http://example.test/", Database: database, Cluster: &host.ClusterOptions{RecoveryGeneration: "invalid"}}
		if database == nil {
			options.DataDir = filepath.Join(t.TempDir(), "untouched")
		}
		app, err := host.Open(context.Background(), options)
		if err == nil {
			app.Close()
			t.Fatal("invalid cluster configuration accepted")
		}
		if options.DataDir != "" {
			if _, err := os.Stat(options.DataDir); !os.IsNotExist(err) {
				t.Fatal("configuration failure touched storage", err)
			}
		}
	}
}

func TestStandaloneRejectsAndClosesPeerListener(t *testing.T) {
	app, err := host.Open(context.Background(), host.Options{PublicURL: "http://example.test/", DataDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ServePeer(listener); err == nil {
		t.Fatal("standalone served peer")
	}
	if conn, err := net.Dial("tcp", listener.Addr().String()); err == nil {
		conn.Close()
		t.Fatal("rejected listener remained open")
	}
}
