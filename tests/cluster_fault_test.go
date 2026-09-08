package tests

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/sdk"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

func waitFault(t *testing.T, ctx context.Context, budget time.Duration, description string, ready func() bool) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for !ready() {
		select {
		case <-bounded.Done():
			t.Fatal(description, bounded.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

type clusterFaultNode struct {
	site, publicAddress, peerAddress string
	database, peer                   *faultProxy
	process                          *hostTestProcess
	args                             []string
	log                              *os.File
}

func (n *clusterFaultNode) readiness(ctx context.Context) (host.Readiness, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", n.site+"health/ready", nil)
	if err != nil {
		return host.Readiness{}, err
	}
	response, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return host.Readiness{}, err
	}
	defer response.Body.Close()
	var status host.Readiness
	err = json.NewDecoder(response.Body).Decode(&status)
	return status, err
}

func (n *clusterFaultNode) start(t *testing.T, ctx context.Context) {
	t.Helper()
	n.process = launchHostTestProcess(t, n.log, n.args...)
	waitFault(t, ctx, 10*time.Second, "cluster node did not become ready", func() bool {
		status, err := n.readiness(ctx)
		return err == nil && status.Accepting
	})
}

func (n *clusterFaultNode) awaitFailure(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-n.process.done:
		n.process.stopped = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(8 * time.Second):
		t.Fatal("database-isolated node kept serving")
	}
	if status, err := n.readiness(ctx); err == nil && status.Serving {
		t.Fatal("failed node still reports serving")
	}
}

func newClusterFaultNode(t *testing.T, ctx context.Context, dir, databaseURL, recovery string, ca *testcert.Authority) *clusterFaultNode {
	t.Helper()
	must(t, os.Mkdir(dir, 0700))
	parsed, err := pgx.ParseConfig(databaseURL)
	must(t, err)
	if parsed.Host != "localhost" && !net.ParseIP(parsed.Host).IsLoopback() {
		t.Skip("network partition regression requires a loopback PostgreSQL test server")
	}
	dbProxy := newFaultProxy(t, ctx, net.JoinHostPort(parsed.Host, strconv.Itoa(int(parsed.Port))))
	public, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	peer, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	publicAddress, peerAddress := public.Addr().String(), peer.Addr().String()
	peerProxy := newFaultProxy(t, ctx, peerAddress)
	n := &clusterFaultNode{site: "http://" + publicAddress + "/dune/", publicAddress: publicAddress, peerAddress: "https://" + peerProxy.address() + "/private/peer", database: dbProxy, peer: peerProxy}
	writeYAML := func(name string, value any) string {
		path := filepath.Join(dir, name)
		data, err := yaml.Marshal(value)
		must(t, err)
		must(t, os.WriteFile(path, data, 0600))
		return path
	}
	cert := ca.Issue(t, "127.0.0.1", nil)
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	must(t, err)
	for name, data := range map[string][]byte{
		"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate.Raw}),
	} {
		must(t, os.WriteFile(filepath.Join(dir, name), data, 0600))
	}
	address, err := url.Parse(databaseURL)
	must(t, err)
	_, port, err := net.SplitHostPort(dbProxy.address())
	must(t, err)
	// Retain the database's original TLS hostname and credentials; only the
	// local port changes. Query overrides must not bypass the fault proxy.
	address.Host = net.JoinHostPort(parsed.Host, port)
	query := address.Query()
	query.Del("host")
	query.Del("port")
	address.RawQuery = query.Encode()
	databaseFile := writeYAML("database.yaml", map[string]any{"postgres": map[string]string{"url": address.String()}})
	clusterFile := writeYAML("cluster.yaml", map[string]any{"recovery_generation": recovery, "peer": map[string]string{"listen": peerAddress, "address": n.peerAddress, "certificate": "cert.pem", "key": "key.pem", "ca": "ca.pem"}})
	localFile := filepath.Join(dir, "local.yaml")
	must(t, config.Create(localFile, config.Config{Gateway: "ws://" + publicAddress + "/dune/tunnel", Listen: publicAddress, Token: strings.Repeat("x", 32), Target: "unused"}))
	n.log, err = os.Create(filepath.Join(dir, "node.log"))
	must(t, err)
	t.Cleanup(func() {
		n.log.Close()
		data, _ := os.ReadFile(n.log.Name())
		if strings.Contains(string(data), "DATA RACE") {
			t.Error("cluster node data race")
		}
		if t.Failed() {
			t.Log(string(data))
		}
	})
	must(t, public.Close())
	must(t, peer.Close())
	n.args = []string{"--config", localFile, "web", "--url", n.site, "--database-config", databaseFile, "--cluster-config", clusterFile, "--drain-timeout", "0"}
	n.start(t, ctx)
	return n
}

func TestPostgresClusterNetworkPartitions(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	store, err := metadata.Open(ctx, database)
	must(t, err)
	defer store.Close()
	local := identity.NewLocal(store, true)
	user, cookie, err := local.Register(ctx, "partition@example.test", "partition-test-password")
	must(t, err)
	enrollment, _, err := store.IssueEnrollment(ctx, user.ID, "partition machine")
	must(t, err)
	machine, credential, err := store.Enroll(ctx, enrollment, "linux", "amd64")
	must(t, err)
	authorizer := authorization.NewLocal(ctx, local, store)
	recovery, ca, dir := wire.ID(), testcert.New(t), t.TempDir()
	directory, err := store.ConnectionDirectory(ctx, recovery)
	must(t, err)
	var nodes [3]*clusterFaultNode
	for i := range nodes {
		nodes[i] = newClusterFaultNode(t, ctx, filepath.Join(dir, fmt.Sprint(i)), postgresWorkbenchURL(t, database), recovery, ca)
	}
	entry, owner, replacement := nodes[0], nodes[1], nodes[2]
	ingress := newFaultProxy(t, ctx, owner.publicAddress)
	endpoint := "ws://" + ingress.address() + "/dune/tunnel"
	machineFile, sessions := filepath.Join(dir, "machine.yaml"), filepath.Join(dir, "sessions")
	must(t, config.Create(machineFile, config.Config{Gateway: endpoint, Token: credential, Target: machine.ID, SessionDir: sessions}))
	log, err := os.Create(filepath.Join(dir, "fabricd.log"))
	must(t, err)
	defer log.Close()
	fabric := launchHostTestProcess(t, log, "--config", machineFile, "fabricd")
	defer func() {
		fabric.stop(t, syscall.SIGTERM)
		manager, err := tmux.Open(sessions)
		if err == nil {
			manager.Close()
		}
	}()
	observe := func(node *clusterFaultNode) gateway.RouteLease {
		t.Helper()
		var route gateway.RouteLease
		waitFault(t, ctx, 25*time.Second, "expected owner not published", func() bool {
			var err error
			route, err = directory.Resolve(ctx, machine.ID)
			return err == nil && route.Published && route.ValidFor > 0 && route.OwnerAddress == node.peerAddress
		})
		return route
	}
	dial := func(node *clusterFaultNode) *sdk.Client {
		t.Helper()
		grant, err := authorizer.Client(ctx, cookie, machine.ID)
		must(t, err)
		defer grant.Close()
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		client, err := sdk.Dial(bounded, sdk.Options{Gateway: strings.Replace(node.site, "http", "ws", 1) + "tunnel", Target: machine.ID, Token: grant.Token()})
		must(t, err)
		t.Cleanup(func() { client.Close() })
		return client
	}
	original := observe(owner)
	client := dial(entry)
	runtime, terminal, err := client.Start(ctx, profile(dir, "pty", "/bin/sh"))
	must(t, err)
	terminal.Close()
	checkPTY := func(client *sdk.Client, marker string) {
		t.Helper()
		current, err := client.Get(ctx, runtime)
		must(t, err)
		if current.State != "running" || current.Incarnation != runtime.Incarnation {
			t.Fatal("network fault destroyed PTY")
		}
		stream, err := client.Attach(ctx, runtime, false)
		must(t, err)
		defer stream.Close()
		_, err = stream.Input([]byte("printf '" + marker + "\\n'\n"))
		must(t, err)
		receive(t, stream, "data", marker)
	}
	checkPTY(client, "PARTITION_INITIAL_OK")

	// Lose a response after a real side effect has been admitted. The owner
	// completes it while both directions of its peer transport remain stalled.
	peerConnections := owner.peer.accepted.Load()
	callCtx, stopCall := context.WithTimeout(ctx, 3*time.Second)
	defer stopCall()
	result := make(chan error, 1)
	go func() {
		_, err := client.Exec(callCtx, api.Exec{WorkingDirectory: dir, Command: api.Command{Argv: []string{"/bin/sh", "-c", "printf started > exec-start; while [ ! -f exec-release ]; do sleep 0.02; done; printf once >> exec-count"}, TimeoutSeconds: 10}})
		result <- err
	}()
	waitFault(t, ctx, time.Second, "command was not admitted", func() bool { _, err := os.Stat(filepath.Join(dir, "exec-start")); return err == nil })
	owner.peer.block(true)
	must(t, os.WriteFile(filepath.Join(dir, "exec-release"), nil, 0600))
	waitFault(t, ctx, time.Second, "command did not finish during peer partition", func() bool { b, _ := os.ReadFile(filepath.Join(dir, "exec-count")); return string(b) == "once" })
	waitFault(t, ctx, time.Second, "peer proxy did not retain traffic", func() bool { return owner.peer.stalled.Load() > 0 })
	var failure *api.Error
	if err := <-result; !errors.As(err, &failure) || failure.Code != "RESULT_UNKNOWN" {
		t.Fatal("lost result did not remain unknown", err)
	}
	if owner.peer.accepted.Load() != peerConnections {
		t.Fatal("entry redialed an uncertain peer request")
	}
	owner.peer.block(false)
	client.Close()
	client = dial(entry) // Explicit new user connection after the uncertain call.
	checkPTY(client, "PARTITION_PEER_RECOVERED")
	if current := observe(owner); current.Epoch != original.Epoch || !reflect.DeepEqual(current.Binding, original.Binding) {
		t.Fatal("peer fault changed machine ownership")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "exec-count")); err != nil || string(b) != "once" {
		t.Fatal("uncertain side effect replayed", err)
	}

	// Isolate only the entry's SQL pool. Another entry uses the same owner and
	// unchanged execution binding; this is a new connection, not socket migration.
	entry.database.block(true)
	entry.awaitFailure(t, ctx)
	if _, err := client.Get(ctx, runtime); err == nil {
		t.Fatal("old entry kept accepting work after losing admission")
	}
	client.Close()
	client = dial(replacement)
	checkPTY(client, "PARTITION_ENTRY_RECOVERED")
	if current := observe(owner); current.Epoch != original.Epoch {
		t.Fatal("entry failure replaced healthy owner")
	}
	entry.database.block(false)
	entry.start(t, ctx)
	client.Close()
	client = dial(entry)

	// Isolate the owner from SQL. It must stop; no failed renewal or release
	// can extend authority. Route the reconnecting, uninterrupted fabricd to C.
	owner.database.block(true)
	owner.awaitFailure(t, ctx)
	if _, err := client.Get(ctx, runtime); err == nil {
		t.Fatal("old owner stream survived admission failure")
	}
	client.Close()
	ingress.route(replacement.publicAddress)
	current := observe(replacement)
	if current.Epoch <= original.Epoch || current.OwnerBootID == original.OwnerBootID || current.Binding.Incarnation != original.Binding.Incarnation || current.Binding.Generation <= original.Binding.Generation {
		t.Fatal("owner replacement lost fencing or restarted fabricd")
	}
	client = dial(entry)
	checkPTY(client, "PARTITION_OWNER_RECOVERED")
	owner.database.block(false)
	owner.start(t, ctx)
	if route := observe(replacement); route.Epoch != current.Epoch || route.OwnerBootID != current.OwnerBootID {
		t.Fatal("restarted old node displaced current owner")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "exec-count")); err != nil || string(b) != "once" {
		t.Fatal("side effect replayed across owner replacement", err)
	}
	must(t, client.Stop(ctx, runtime))
}
