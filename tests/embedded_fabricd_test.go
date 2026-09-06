package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/sdk"
	"github.com/aiomni/dune/pkg/transport/tunnel"
)

func TestExternalFabricdHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repo, err := filepath.Abs("..")
	must(t, err)
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join(repo, "samples/fabricd/main.go"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "main.go"), source, 0600))
	// Build under an unrelated module path: importing a Dune internal package
	// here would be rejected by Go, even when a public signature concealed it.
	module := fmt.Sprintf("module external.example/host\n\ngo 1.27.0\n\nrequire github.com/aiomni/dune v0.0.0\nreplace github.com/aiomni/dune => %q\n", repo)
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0600))
	hostBinary := filepath.Join(dir, "host")
	build := exec.CommandContext(ctx, "go", "build", "-mod=mod", "-race", "-o", hostBinary, ".")
	build.Dir = dir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("external host build: %v\n%s", err, output)
	}
	g := gateway.New()
	defer g.Close()
	server := httptest.NewServer(tunnel.NewHandler(ctx, g, func(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		role := gateway.RoleSDK
		if token == "machine-token" {
			role = gateway.RoleDaemon
		} else if token != "client-token" {
			return gateway.BindingContext{}, nil, fmt.Errorf("unauthorized")
		}
		return (access.Grant{Target: "machine", Role: role}).Bind()
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/tunnel"
	log, err := os.Create(filepath.Join(dir, "host.log"))
	must(t, err)
	defer log.Close()
	command := exec.CommandContext(ctx, hostBinary)
	command.Env = append(os.Environ(), "DUNE_GATEWAY="+endpoint, "DUNE_TOKEN=machine-token", "DUNE_TARGET=machine", "DUNE_STATE_DIR="+filepath.Join(dir, "state"))
	command.Stdout, command.Stderr = log, log
	must(t, command.Start())
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		cancel()
		<-done
		output, _ := os.ReadFile(log.Name())
		if strings.Contains(string(output), "DATA RACE") {
			t.Errorf("host data race: %s", output)
		}
		if t.Failed() {
			t.Log(string(output))
		}
	}()
	var client *sdk.Client
	for client == nil {
		client, err = sdk.Dial(ctx, sdk.Options{Gateway: endpoint, Token: "client-token", Target: "machine"})
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("external host did not connect: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer client.Close()
	result, err := client.Exec(ctx, api.Exec{Command: api.Command{Argv: []string{"/bin/echo", "external-host"}}, WorkingDirectory: dir})
	must(t, err)
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "external-host" {
		t.Fatalf("external host execution = %+v", result)
	}
	data := []byte("external upload")
	digest := sha256.Sum256(data)
	path := filepath.Join(dir, "uploaded")
	upload, err := client.Upload(ctx, api.Upload{Action: "create", Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])})
	must(t, err)
	_, err = client.Upload(ctx, api.Upload{Action: "chunk", ID: upload.ID, Data: data})
	must(t, err)
	_, err = client.Upload(ctx, api.Upload{Action: "commit", ID: upload.ID})
	must(t, err)
	actual, err := os.ReadFile(path)
	must(t, err)
	if string(actual) != string(data) {
		t.Fatal("external host upload corrupted")
	}
}
