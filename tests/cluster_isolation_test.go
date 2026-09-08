package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/sdk"
)

type clusterIsolationMachine struct {
	user       identity.User
	cookie     string
	target     string
	credential string
	work       string
	sessions   string
	process    *hostTestProcess
	log        *os.File
}

func TestPostgresClusterTwoUserIsolationAndCapabilities(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := metadata.Open(ctx, database)
	must(t, err)
	defer store.Close()
	local := identity.NewLocal(store, true)
	dir := t.TempDir()
	newMachine := func(label string) clusterIsolationMachine {
		t.Helper()
		user, cookie, err := local.Register(ctx, label+"@cluster.test", "cluster-isolation-password")
		must(t, err)
		enrollment, _, err := store.IssueEnrollment(ctx, user.ID, label+" machine")
		must(t, err)
		machine, credential, err := store.Enroll(ctx, enrollment, "linux", "amd64")
		must(t, err)
		work := filepath.Join(dir, "work-"+label)
		must(t, os.Mkdir(work, 0700))
		return clusterIsolationMachine{user: user, cookie: cookie, target: machine.ID, credential: credential, work: work, sessions: filepath.Join(dir, "sessions-"+label)}
	}
	machines := []clusterIsolationMachine{newMachine("alice"), newMachine("bob")}

	recovery, ca := wire.ID(), testcert.New(t)
	directory, err := store.ConnectionDirectory(ctx, recovery)
	must(t, err)
	nodes := make([]*clusterFaultNode, 3)
	for i := range nodes {
		nodes[i] = newClusterFaultNode(t, ctx, filepath.Join(dir, "node-"+string(rune('a'+i))), postgresWorkbenchURL(t, database), recovery, ca)
	}
	for i := range machines {
		endpoint := strings.Replace(nodes[i+1].site, "http", "ws", 1) + "tunnel"
		path := filepath.Join(dir, "machine-"+machines[i].user.ID+".yaml")
		must(t, config.Create(path, config.Config{Gateway: endpoint, Token: machines[i].credential, Target: machines[i].target, SessionDir: machines[i].sessions}))
		machines[i].log, err = os.Create(filepath.Join(dir, "fabricd-"+machines[i].user.ID+".log"))
		must(t, err)
		machines[i].process = launchHostTestProcess(t, machines[i].log, "--config", path, "fabricd")
	}
	defer func() {
		for i := range machines {
			machines[i].process.stop(t, syscall.SIGTERM)
			manager, err := tmux.Open(machines[i].sessions)
			if err == nil {
				manager.Close()
			}
			machines[i].log.Close()
			if data, _ := os.ReadFile(machines[i].log.Name()); strings.Contains(string(data), "DATA RACE") {
				t.Error("fabricd reported a data race")
			}
		}
	}()

	for i := range machines {
		expectedOwner := nodes[i+1].peerAddress
		waitFault(t, ctx, 25*time.Second, "machine owner did not publish", func() bool {
			route, err := directory.Resolve(ctx, machines[i].target)
			return err == nil && route.Published && route.ValidFor > 0 && route.OwnerAddress == expectedOwner
		})
	}
	authorizer := authorization.NewLocal(ctx, local, store)
	for i := range machines {
		page, err := authorizer.Discover(ctx, machines[i].user, runner.Query{}, true)
		if err != nil || len(page.Items) != 1 || page.Items[0].Runner.Binding == nil || page.Items[0].Runner.Binding.MachineID != machines[i].target {
			t.Fatal("user discovery crossed machine ownership", i, page, err)
		}
		other := machines[1-i]
		if grant, err := authorizer.Client(ctx, machines[i].cookie, other.target); !errors.Is(err, authorization.ErrNotFound) {
			if grant != nil {
				grant.Close()
			}
			t.Fatal("user received access to another owner's machine", i, err)
		}
	}

	entry := strings.Replace(nodes[0].site, "http", "ws", 1) + "tunnel"
	clients := make([]*sdk.Client, len(machines))
	for i := range machines {
		grant, err := authorizer.Client(ctx, machines[i].cookie, machines[i].target)
		must(t, err)
		// A valid one-time credential is bound to one target. Presenting it for
		// the other user's target fails closed and consumes the one-time grant.
		wrong, err := sdk.Dial(ctx, sdk.Options{Gateway: entry, Token: grant.Token(), Target: machines[1-i].target})
		if wrong != nil {
			wrong.Close()
		}
		if err == nil {
			t.Fatal("target-bound credential crossed machine tunnels", i)
		}
		if replay, err := sdk.Dial(ctx, sdk.Options{Gateway: entry, Token: grant.Token(), Target: machines[i].target}); err == nil {
			replay.Close()
			t.Fatal("failed target handshake left a one-time credential replayable", i)
		}
		grant.Close()
		grant, err = authorizer.Client(ctx, machines[i].cookie, machines[i].target)
		must(t, err)
		clients[i], err = sdk.Dial(ctx, sdk.Options{Gateway: entry, Token: grant.Token(), Target: machines[i].target})
		grant.Close()
		must(t, err)
		defer clients[i].Close()
	}

	exercise := func(i int) {
		t.Helper()
		client, machine := clients[i], machines[i]
		label := strings.ToUpper(machine.user.Email[:strings.IndexByte(machine.user.Email, '@')])
		file := filepath.Join(machine.work, "probe.txt")
		must(t, client.Files(ctx, api.File{Action: "write", Path: file, Data: []byte(label)}, nil))
		var read struct{ Data []byte }
		must(t, client.Files(ctx, api.File{Action: "read", Path: file, Length: len(label)}, &read))
		if string(read.Data) != label {
			t.Fatal("file operation reached the wrong machine", i, string(read.Data))
		}
		result, err := client.Exec(ctx, api.Exec{WorkingDirectory: machine.work, Command: api.Command{Argv: []string{"git", "init", "-q", "-b", "main"}}})
		if err != nil || result.ExitCode != 0 {
			t.Fatal("git initialization failed", i, result, err)
		}
		status, err := client.Git(ctx, api.Git{Action: "status", Directory: machine.work})
		if err != nil || status.ExitCode != 0 || len(status.Entries) != 1 || status.Entries[0].Path != "probe.txt" {
			t.Fatal("git operation did not traverse the selected owner", i, status, err)
		}

		pty, stream, err := client.Start(ctx, profile(machine.work, "pty", "/bin/sh"))
		must(t, err)
		_, err = stream.Input([]byte("printf '" + label + "_PTY_OK\\n'\n"))
		must(t, err)
		receive(t, stream, "data", label+"_PTY_OK")
		stream.Close()
		must(t, client.Stop(ctx, pty))

		acp, stream, err := client.Start(ctx, profile(machine.work, "acp", "/bin/cat"))
		must(t, err)
		_, err = stream.Input([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + strings.ToLower(label) + `/probe"}`))
		must(t, err)
		receive(t, stream, "data", strings.ToLower(label)+"/probe")
		stream.Close()
		must(t, client.Stop(ctx, acp))
	}
	exercise(0)
	exercise(1)
}
