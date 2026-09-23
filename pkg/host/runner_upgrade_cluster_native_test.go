package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/transport/peer"
	"github.com/aiomni/dune/pkg/upgrade"
	"github.com/jackc/pgx/v5"
)

const nativeUpgradeCookie = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type nativePeerConfig struct {
	Database, PublicAddress, PeerAddress, Ready string
	Root, Certificate, Key                      []byte
	Manifests                                   []upgrade.Manifest
}

// The child runs the ordinary host HTTP and authenticated peer handlers. SIGKILL
// therefore interrupts a real host process, including its pools and sockets.
func TestNativeUpgradePeerProcess(t *testing.T) {
	path := os.Getenv("DUNE_NATIVE_UPGRADE_PEER")
	if path == "" {
		t.Skip("native upgrade child only")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c nativePeerConfig
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	private, err := x509.ParsePKCS8PrivateKey(c.Key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	source, err := upgrade.NewCatalog(c.Manifests)
	if err != nil {
		t.Fatal(err)
	}
	app, err := Open(t.Context(), Options{PublicURL: "http://" + c.PublicAddress, Database: &storage.Config{Postgres: &storage.Postgres{URL: c.Database}}, UpgradeSource: source, Cluster: &ClusterOptions{Peer: peer.Config{Address: "https://" + c.PeerAddress + peer.EndpointPath, Certificate: tls.Certificate{Certificate: [][]byte{c.Certificate}, PrivateKey: private}, Roots: roots}}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	public, err := net.Listen("tcp", c.PublicAddress)
	if err != nil {
		t.Fatal(err)
	}
	privateListener, err := net.Listen("tcp", c.PeerAddress)
	if err != nil {
		t.Fatal(err)
	}
	go app.ServePeer(privateListener)
	if err := os.WriteFile(c.Ready, []byte(app.core.BootID()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.Serve(public); err != nil {
		t.Fatal(err)
	}
}

type nativeUpgradeCluster struct {
	peerAddress string
	config      nativePeerConfig
	configPath  string
	process     *exec.Cmd
	log         *os.File
	proxy       *httputil.ReverseProxy
	service     nativeUpgradeHTTP
}

func newNativeUpgradeCluster(t *testing.T, ctx context.Context, options *Options, manifests map[string]upgrade.Manifest) *nativeUpgradeCluster {
	t.Helper()
	address := os.Getenv("DUNE_TEST_POSTGRES")
	if address == "" {
		return nil
	}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	admin, err := pgx.Connect(ctx, address)
	check(err)
	schema := "dune_upgrade_" + wire.ID()
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	check(err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close(cleanup)
	})
	database, err := url.Parse(address)
	check(err)
	parameters := database.Query()
	parameters.Set("search_path", schema)
	database.RawQuery = parameters.Encode()
	available := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		defer listener.Close()
		return listener.Addr().String()
	}
	ca := testcert.New(t)
	cluster := &nativeUpgradeCluster{peerAddress: available()}
	options.DataDir = ""
	options.Database = &storage.Config{Postgres: &storage.Postgres{URL: database.String()}}
	options.Cluster = &ClusterOptions{Peer: peer.Config{Address: "https://" + cluster.peerAddress + peer.EndpointPath, Certificate: ca.Issue(t, "127.0.0.1", nil), Roots: ca.Roots()}}
	directory := t.TempDir()
	certificate := ca.Issue(t, "127.0.0.1", nil)
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	check(err)
	cluster.config = nativePeerConfig{Database: database.String(), PublicAddress: available(), PeerAddress: available(), Ready: filepath.Join(directory, "ready"), Root: ca.Certificate.Raw, Certificate: certificate.Certificate[0], Key: key}
	for _, m := range manifests {
		cluster.config.Manifests = append(cluster.config.Manifests, m)
	}
	cluster.configPath = filepath.Join(directory, "config.json")
	body, err := json.Marshal(cluster.config)
	check(err)
	check(os.WriteFile(cluster.configPath, body, 0600))
	cluster.log, err = os.Create(filepath.Join(directory, "process.log"))
	check(err)
	endpoint, err := url.Parse("http://" + cluster.config.PublicAddress)
	check(err)
	cluster.proxy = httputil.NewSingleHostReverseProxy(endpoint)
	cluster.service = nativeUpgradeHTTP{endpoint: endpoint.String(), client: &http.Client{Timeout: 25 * time.Second}, cookie: nativeUpgradeCookie}
	t.Cleanup(func() {
		cluster.kill(t)
		cluster.log.Close()
		if t.Failed() {
			body, _ := os.ReadFile(cluster.log.Name())
			t.Log(string(body))
		}
	})
	cluster.start(t, ctx)
	return cluster
}

func (c *nativeUpgradeCluster) serveEntry(t *testing.T, app *App) {
	t.Helper()
	listener, err := net.Listen("tcp", c.peerAddress)
	if err != nil {
		t.Fatal(err)
	}
	go app.ServePeer(listener)
}

func (c *nativeUpgradeCluster) start(t *testing.T, ctx context.Context) {
	t.Helper()
	_ = os.Remove(c.config.Ready)
	c.process = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeUpgradePeerProcess$", "-test.timeout=8m")
	c.process.Env = append(os.Environ(), "DUNE_NATIVE_UPGRADE_PEER="+c.configPath)
	c.process.Stdout, c.process.Stderr = c.log, c.log
	if err := c.process.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(c.config.Ready); err == nil {
			return
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			t.Fatal("cluster host failed to start")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
func (c *nativeUpgradeCluster) kill(t *testing.T) {
	t.Helper()
	if c.process == nil {
		return
	}
	pid := c.process.Process.Pid
	_ = c.process.Process.Kill()
	_ = c.process.Wait()
	c.process = nil
	t.Logf("SIGKILL host process %d; persisted submissions retained in shared PostgreSQL", pid)
}

// This adapter deliberately uses the real authorized HTTP paths so admission
// originates in the process that the acceptance test later kills.
type nativeUpgradeHTTP struct {
	endpoint string
	cookie   string
	client   *http.Client
}

func (c nativeUpgradeHTTP) call(ctx context.Context, binding runner.Binding, action string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	query := url.Values{"machine_id": {binding.MachineID}, "fabric_id": {binding.FabricID}, "revision": {fmt.Sprint(binding.Revision)}}
	request, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/api/v1/runners/"+binding.RunnerID+"/upgrade/"+action+"?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.endpoint)
	request.Header.Set("X-Dune-Request", "1")
	request.AddCookie(&http.Cookie{Name: "dune_session", Value: c.cookie})
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("upgrade %s returned HTTP %d", action, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(output)
}
func (c nativeUpgradeHTTP) InspectRunner(ctx context.Context, scope upgrade.Scope) (out upgrade.Inspection, err error) {
	err = c.call(ctx, scope.Binding, "inspect", struct{}{}, &out)
	return
}
func (c nativeUpgradeHTTP) PreviewUpgrade(ctx context.Context, scope upgrade.Scope, request upgrade.PreviewRequest) (out upgrade.Preview, err error) {
	err = c.call(ctx, scope.Binding, "preview", request, &out)
	return
}
func (c nativeUpgradeHTTP) StartUpgrade(ctx context.Context, scope upgrade.Scope, request upgrade.Request) (out upgrade.Observation, err error) {
	err = c.call(ctx, scope.Binding, "start", request, &out)
	return
}
func (c nativeUpgradeHTTP) GetUpgrade(ctx context.Context, scope upgrade.Scope, request upgrade.Query) (out upgrade.Observation, err error) {
	err = c.call(ctx, scope.Binding, "get", request, &out)
	return
}
func (c nativeUpgradeHTTP) ListUpgrades(ctx context.Context, scope upgrade.Scope, request upgrade.ListRequest) (out upgrade.History, err error) {
	err = c.call(ctx, scope.Binding, "list", request, &out)
	return
}
