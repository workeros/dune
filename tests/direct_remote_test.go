package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
)

// This opt-in test runs its SDK on the local machine and dials the remote
// Gateway directly; SSH is not part of the application request path.
func TestDirectRemote(t *testing.T) {
	path := os.Getenv("DUNE_REMOTE_CONFIG")
	if path == "" {
		t.Skip("set DUNE_REMOTE_CONFIG for direct remote validation")
	}
	c, e := config.Load(path)
	must(t, e)
	tc, e := c.TLS()
	must(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, e := sdk.Dial(ctx, sdk.Options{Gateway: c.Gateway, Token: c.Token, Target: c.Target, TLSConfig: tc})
	must(t, e)
	defer client.Close()
	result, e := client.Exec(ctx, api.Exec{Command: api.Command{Argv: []string{"uname", "-s"}}, WorkingDirectory: "/tmp"})
	must(t, e)
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "Linux" {
		t.Fatalf("expected remote Linux: %+v", result)
	}
	t.Logf("direct endpoint=%s remote OS=%s", c.Gateway, strings.TrimSpace(result.Stdout))
	bad, e := sdk.Dial(ctx, sdk.Options{Gateway: c.Gateway, Token: "invalid", Target: c.Target, TLSConfig: tc})
	if bad != nil {
		bad.Close()
	}
	if e == nil {
		t.Fatal("bad token accepted")
	}
	result, e = client.Exec(ctx, api.Exec{Command: api.Command{Argv: []string{"mktemp", "-d", "/tmp/dune-direct-XXXXXX"}}, WorkingDirectory: "/tmp"})
	must(t, e)
	dir := strings.TrimSpace(result.Stdout)
	if result.ExitCode != 0 || !strings.HasPrefix(dir, "/tmp/dune-direct-") {
		t.Fatalf("scratch: %+v", result)
	}
	defer func() {
		if e := client.Files(ctx, api.File{Action: "remove", Path: dir, Recursive: true}, nil); e != nil {
			t.Error(e)
		}
	}()
	content := []byte(strings.Repeat("direct-remote", 8192))
	sum := sha256.Sum256(content)
	upload, e := client.Upload(ctx, api.Upload{Action: "create", Path: dir + "/uploaded", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
	must(t, e)
	for upload.Offset < int64(len(content)) {
		end := upload.Offset + 32768
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		upload, e = client.Upload(ctx, api.Upload{Action: "chunk", ID: upload.ID, Offset: upload.Offset, Data: content[upload.Offset:end]})
		must(t, e)
	}
	upload, e = client.Upload(ctx, api.Upload{Action: "commit", ID: upload.ID})
	must(t, e)
	if !upload.Committed {
		t.Fatal("upload did not commit")
	}
	var read struct{ Data []byte }
	must(t, client.Files(ctx, api.File{Action: "read", Path: dir + "/uploaded", Length: 12}, &read))
	if string(read.Data) != "direct-remot" {
		t.Fatal(string(read.Data))
	}
	result, e = client.Exec(ctx, api.Exec{Command: api.Command{Argv: []string{"git", "init"}}, WorkingDirectory: dir})
	must(t, e)
	if result.ExitCode != 0 {
		t.Fatal(result)
	}
	status, e := client.Git(ctx, api.Git{Action: "status", Directory: dir})
	must(t, e)
	if status.ExitCode != 0 || len(status.Entries) != 1 || status.Entries[0].Path != "uploaded" {
		t.Fatal(status)
	}
	rt, stream, e := client.Start(ctx, profile(dir, "pty", "/bin/sh", "-c", "printf 'REMOTE_READY\\n'; read value; printf 'REPLY:%s\\n' \"$value\""))
	must(t, e)
	defer stream.Close()
	defer client.Stop(ctx, rt)
	receive(t, stream, "data", "REMOTE_READY")
	_, e = stream.Input([]byte("hello-from-local\n"))
	must(t, e)
	receive(t, stream, "data", "REPLY:hello-from-local")
	receive(t, stream, "exit", "")
	rt, stream, e = client.Start(ctx, profile(dir, "acp", "/bin/cat"))
	must(t, e)
	defer stream.Close()
	defer client.Stop(ctx, rt)
	_, e = stream.Input([]byte(`{"jsonrpc":"2.0","id":1,"method":"direct/probe"}`))
	must(t, e)
	receive(t, stream, "data", "direct/probe")
	must(t, client.Stop(ctx, rt))
	script := "import socket\ns=socket.socket();s.bind(('127.0.0.1',0));s.listen(1)\nprint('PORT:%d'%s.getsockname()[1],flush=True)\nc,_=s.accept()\nwhile True:\n b=c.recv(32768)\n if not b: break\n c.sendall(b)\nc.close();s.close()"
	rt, stream, e = client.Start(ctx, profile(dir, "pty", "python3", "-u", "-c", script))
	must(t, e)
	defer stream.Close()
	defer client.Stop(ctx, rt)
	output := ""
	port := 0
	for port == 0 {
		m, e := stream.Recv()
		must(t, e)
		if m.Kind == "data" {
			output += string(m.Data)
			match := regexp.MustCompile(`PORT:(\d+)[\r\n]`).FindStringSubmatch(output)
			if len(match) > 1 {
				port, _ = strconv.Atoi(match[1])
			}
		}
	}
	conn, e := client.Connect(ctx, port)
	must(t, e)
	defer conn.Close()
	_, e = conn.Write([]byte("tcp-from-local"))
	must(t, e)
	must(t, conn.CloseWrite())
	data, e := io.ReadAll(conn)
	must(t, e)
	must(t, conn.Finish())
	if string(data) != "tcp-from-local" {
		t.Fatal(string(data))
	}
	t.Log(fmt.Sprintf("PASS: HTTP token auth, Exec, Files, %d-byte upload, Git, PTY, ACP, TCP", len(content)))
}
