package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/transport/peer"
	"github.com/fasthttp/websocket"
)

func TestPrefixedWorkbenchEnrollmentAndTerminal(t *testing.T) {
	t.Run("default", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{}) })
	t.Run("runner-entry", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{runnerEntry: true}) })
	t.Run("machine-entry-override", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{override: true}) })
	t.Run("external-host", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{external: true}) })
}

type workbenchCase struct {
	override, external bool
	cluster            bool
	enterprise         bool
	runnerEntry        bool
	database           *storage.Config
}

func testPrefixedWorkbench(t *testing.T, mode workbenchCase) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	site := origin + "/tools/dune/"
	onlineSite := site
	var app *host.App
	options := host.Options{PublicURL: site, DataDir: filepath.Join(dir, "accounts")}
	var sharedPrincipal atomic.Value
	var policyRevoked, sharedExecution atomic.Bool
	if mode.enterprise {
		options.AccessChecker = enterpriseCheck(func(ctx context.Context, r access.Request) (access.Decision, error) {
			shared, _ := sharedPrincipal.Load().(string)
			allowed := r.PrincipalID == r.OwnerID || (shared != "" && r.PrincipalID == shared)
			if shared != "" && r.PrincipalID == shared && r.PrincipalID != r.OwnerID && (r.Operation == "profile.start" || r.Operation == "exec") {
				sharedExecution.Store(true)
			}
			allowed = allowed && !policyRevoked.Load() && !(r.Operation == "files" && r.Suboperation == "write") && !(r.Operation == "agent.config" && r.Suboperation == "save")
			return access.Decision{Allowed: allowed, Reason: "ENTERPRISE_POLICY", ID: r.RequestID, ValidUntil: time.Now().Add(time.Second)}, nil
		})
		defer func() {
			if !sharedExecution.Load() {
				t.Error("shared execution did not reach the enterprise operation checker")
			}
		}()
	}
	if mode.database != nil {
		options.DataDir, options.Database = "", mode.database
	}
	var ownerGateway string
	var peerListener net.Listener
	var newCluster func() (*host.ClusterOptions, net.Listener)
	if mode.cluster {
		ca := testcert.New(t)
		newCluster = func() (*host.ClusterOptions, net.Listener) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			must(t, err)
			t.Cleanup(func() { listener.Close() })
			return &host.ClusterOptions{Peer: peer.Config{
				Address:     "https://" + listener.Addr().String() + "/private/peer",
				Certificate: ca.Issue(t, "127.0.0.1", nil), Roots: ca.Roots(),
			}}, listener
		}
		options.Cluster, peerListener = newCluster()
	}
	servePeer := func(application *host.App, listener net.Listener) {
		done := make(chan error, 1)
		go func() { done <- application.ServePeer(listener) }()
		t.Cleanup(func() {
			application.Close()
			select {
			case err := <-done:
				if err != http.ErrServerClosed {
					t.Errorf("peer listener: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("peer listener remained open")
			}
		})
	}
	if mode.cluster {
		if mode.database == nil {
			t.Fatal("cluster workbench requires PostgreSQL")
		}
		remote := httptest.NewUnstartedServer(nil)
		remoteSite := "http://" + remote.Listener.Addr().String() + "/tools/dune/"
		remoteOptions := host.Options{PublicURL: remoteSite, Database: mode.database, AccessChecker: options.AccessChecker}
		var remotePeer net.Listener
		remoteOptions.Cluster, remotePeer = newCluster()
		remoteApp, err := host.Open(ctx, remoteOptions)
		must(t, err)
		defer remoteApp.Close()
		servePeer(remoteApp, remotePeer)
		remote.Config.Handler = remoteApp
		remote.Start()
		defer remote.Close()
		ownerGateway = "ws" + strings.TrimPrefix(remoteSite, "http") + "tunnel"
		// Keep all user entry points on A. Only fabricd connects to owner B.
		onlineSite = site
	}
	if mode.override {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/private/connect" {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/tools/dune/tunnel"
			r.URL.RawPath = ""
			app.ServeHTTP(w, r)
		}))
		defer proxy.Close()
		options.GatewayURL = "ws" + strings.TrimPrefix(proxy.URL, "http") + "/private/connect"
	}
	if mode.external {
		address := server.Listener.Addr().String()
		must(t, server.Listener.Close())
		stop := externalWorkbench(t, ctx, address, options)
		defer stop()
	} else {
		var err error
		app, err = host.Open(ctx, options)
		must(t, err)
		defer app.Close()
		if mode.cluster {
			servePeer(app, peerListener)
		}
		server.Config.Handler = app
		server.Start()
		defer server.Close()
	}
	jar, err := cookiejar.New(nil)
	must(t, err)
	browser := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	do := func(method, route string, body, out any) {
		t.Helper()
		data, err := json.Marshal(body)
		must(t, err)
		request, err := http.NewRequestWithContext(ctx, method, site+route, bytes.NewReader(data))
		must(t, err)
		request.Header.Set("Origin", origin)
		request.Header.Set("X-Dune-Request", "1")
		request.Header.Set("Content-Type", "application/json")
		response, err := browser.Do(request)
		must(t, err)
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 201 {
			t.Fatalf("%s %s: %d", method, route, response.StatusCode)
		}
		if out != nil {
			must(t, json.NewDecoder(response.Body).Decode(out))
		}
	}
	var user struct{ ID string }
	email := "prefix@example.test"
	do("POST", "api/auth/register", map[string]string{"email": email, "password": "prefix-test-password"}, &user)
	var enrollment struct{ Token string }
	do("POST", "api/enrollments", map[string]string{"name": "prefixed machine"}, &enrollment)
	machinePath := filepath.Join(dir, "machine", "config.yaml")
	must(t, webapp.EnrollMachine(ctx, machinePath, site, enrollment.Token, ""))
	machineConfig, err := config.Load(machinePath)
	must(t, err)
	if mode.enterprise {
		email = "shared@example.test"
		do("POST", "api/auth/register", map[string]string{"email": email, "password": "prefix-test-password"}, &user)
		sharedPrincipal.Store(user.ID)
	}
	expectedGateway := options.GatewayURL
	if expectedGateway == "" {
		expectedGateway = "ws" + strings.TrimPrefix(site, "http") + "tunnel"
	}
	if machineConfig.Gateway != expectedGateway {
		t.Fatalf("machine address = %s", machineConfig.Gateway)
	}
	if mode.cluster {
		machineConfig.Gateway = ownerGateway
		machinePath = filepath.Join(dir, "machine", "owner.yaml")
		must(t, config.Create(machinePath, machineConfig))
	}
	log, err := os.Create(filepath.Join(dir, "fabricd.log"))
	must(t, err)
	defer log.Close()
	connector := launchHostTestProcess(t, log, "--config", machinePath, "fabricd")
	defer func() {
		connector.stop(t, syscall.SIGTERM)
		manager, err := tmux.Open(machineConfig.SessionDir)
		if err == nil {
			_ = manager.Close()
		}
		output, _ := os.ReadFile(log.Name())
		if strings.Contains(string(output), "DATA RACE") {
			t.Errorf("fabricd race: %s", output)
		}
	}()
	for {
		var page struct {
			Items []struct {
				ID     string
				Online bool
			}
		}
		// Standalone split-host tests observe the actual owner. Cluster tests
		// read shared online facts from A and execute through A's peer route.
		response, err := browser.Get(onlineSite + "api/machines")
		must(t, err)
		if response.StatusCode != 200 {
			response.Body.Close()
			t.Fatalf("machine readiness: %d", response.StatusCode)
		}
		err = json.NewDecoder(response.Body).Decode(&page)
		response.Body.Close()
		must(t, err)
		if len(page.Items) == 1 && page.Items[0].Online {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("prefixed machine tunnel did not connect")
		case <-time.After(25 * time.Millisecond):
		}
	}
	var selected runner.Runner
	if mode.runnerEntry {
		var page runner.Page
		do("GET", "api/runners", nil, &page)
		for _, row := range page.Items {
			if row.Binding != nil && row.Binding.MachineID == machineConfig.Target {
				selected = row
				break
			}
		}
		if selected.Binding == nil {
			t.Fatal("Runner discovery lost selected machine")
		}
	}
	executionRoute := func(suffix string) string {
		if !mode.runnerEntry {
			return "api/machines/" + machineConfig.Target + "/" + suffix
		}
		b := selected.Binding
		query := url.Values{"machine_id": {b.MachineID}, "fabric_id": {b.FabricID}, "revision": {fmt.Sprint(b.Revision)}}
		return "api/runners/" + selected.ID + "/" + suffix + "?" + query.Encode()
	}
	var runtime api.Runtime
	do("POST", executionRoute("sessions"), profile(dir, "pty", "/bin/sh"), &runtime)
	eventURL, err := url.Parse(site + executionRoute("sessions/"+runtime.ID+"/events"))
	must(t, err)
	query := eventURL.Query()
	query.Set("incarnation", runtime.Incarnation)
	query.Set("generation", fmt.Sprint(runtime.Generation))
	eventURL.RawQuery = query.Encode()
	events := eventURL.String()
	// These are deliberately rejected before a WebSocket upgrade. The old ID-only
	// lookup would silently substitute the current execution identity here.
	for _, bad := range []struct {
		key, value, code string
		status           int
	}{
		{"incarnation", "previous-incarnation", "STALE_RUNTIME", 422},
		{"generation", fmt.Sprint(runtime.Generation + 1), "STALE_RUNTIME", 422},
		{"incarnation", "", "INVALID_RUNTIME", 400},
		{"generation", "0", "INVALID_RUNTIME", 400},
	} {
		modified := *eventURL
		query := modified.Query()
		query.Set(bad.key, bad.value)
		modified.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, "GET", modified.String(), nil)
		must(t, err)
		request.Header.Set("Origin", origin)
		response, err := browser.Do(request)
		must(t, err)
		var failure struct{ Code string }
		must(t, json.NewDecoder(response.Body).Decode(&failure))
		response.Body.Close()
		if response.StatusCode != bad.status || failure.Code != bad.code {
			t.Fatal("subscription did not retain selected Runtime identity", bad.key, response.StatusCode, failure.Code)
		}
	}
	connect := func() *websocket.Conn {
		request, err := http.NewRequest("GET", events, nil)
		must(t, err)
		for _, cookie := range jar.Cookies(request.URL) {
			request.AddCookie(cookie)
		}
		request.Header.Set("Origin", origin)
		wsURL, _ := url.Parse(events)
		wsURL.Scheme = "ws"
		var connection *websocket.Conn
		// Creating a PTY releases its initial input subscription asynchronously.
		// As the browser does, retry only subscription to the same Runtime after an
		// explicit INPUT_OWNED refusal; never repeat creation or a submitted input.
		deadline := time.Now().Add(3 * time.Second)
		for {
			var response *http.Response
			connection, response, err = websocket.DefaultDialer.DialContext(ctx, wsURL.String(), request.Header)
			if err == nil {
				break
			}
			if response == nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			var failure struct{ Code string }
			_ = json.Unmarshal(body, &failure)
			if response.StatusCode != 422 || failure.Code != "INPUT_OWNED" || time.Now().After(deadline) {
				t.Fatalf("terminal handshake: %v (%d): %s", err, response.StatusCode, body)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(25 * time.Millisecond):
			}
		}
		return connection
	}
	connection := connect()
	defer func() { connection.Close() }()
	must(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	must(t, connection.WriteJSON(map[string]string{"type": "input", "data": "printf 'PREFIX_TERMINAL_%s\\n' OK\n"}))
	readPrefixedTerminalMarker(t, connection)
	awaitRevocation := func() {
		must(t, connection.SetReadDeadline(time.Now().Add(3*time.Second)))
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
					t.Fatal("idle terminal was not revoked")
				}
				return
			}
		}
	}
	if mode.enterprise {
		body, err := json.Marshal(map[string]any{"operation": "agent.config", "payload": api.AgentConfigRequest{Action: "save", Config: &api.AgentConfig{Name: "denied", Command: "/bin/sh", Adapter: "pty"}}})
		must(t, err)
		request, err := http.NewRequestWithContext(ctx, "POST", site+executionRoute("call"), bytes.NewReader(body))
		must(t, err)
		request.Header.Set("Origin", origin)
		request.Header.Set("X-Dune-Request", "1")
		request.Header.Set("Content-Type", "application/json")
		response, err := browser.Do(request)
		must(t, err)
		var failure struct{ Code string }
		must(t, json.NewDecoder(response.Body).Decode(&failure))
		response.Body.Close()
		if response.StatusCode != 422 || failure.Code != "ACCESS_DENIED" {
			t.Fatal("Web write bypassed enterprise checker", response.StatusCode, failure.Code)
		}
		var configs []api.AgentConfig
		do("POST", executionRoute("call"), map[string]any{"operation": "agent.config", "payload": api.AgentConfigRequest{Action: "list"}}, &configs)
		if len(configs) != 0 {
			t.Fatal("denied Web write saved configuration")
		}
		policyRevoked.Store(true)
		awaitRevocation()
		response, err = browser.Get(site + "api/machines")
		must(t, err)
		var page struct{ Items []json.RawMessage }
		must(t, json.NewDecoder(response.Body).Decode(&page))
		response.Body.Close()
		if response.StatusCode != 200 || len(page.Items) != 0 {
			t.Fatal("enterprise denial fell back to owner discovery")
		}
		policyRevoked.Store(false)
		connection.Close()
		connection = connect()
		readPrefixedTerminalMarker(t, connection)
	}
	if app != nil {
		started := time.Now()
		must(t, app.SetUserEnabled(ctx, user.ID, false))
		awaitRevocation()
		t.Logf("principal suspension closed idle terminal in %s", time.Since(started))
		for _, reenable := range []bool{false, true} {
			if reenable {
				must(t, app.SetUserEnabled(ctx, user.ID, true))
			}
			response, err := browser.Get(site + "api/me")
			must(t, err)
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatal("suspended session revived")
			}
		}
		do("POST", "api/auth/login", map[string]string{"email": email, "password": "prefix-test-password"}, nil)
		var remaining []api.Runtime
		do("POST", executionRoute("call"), map[string]any{"operation": "runtime.list", "payload": struct{}{}}, &remaining)
		if len(remaining) != 1 || remaining[0].ID != runtime.ID || remaining[0].Incarnation != runtime.Incarnation || remaining[0].Generation != runtime.Generation || remaining[0].State != "running" {
			t.Fatal("principal suspension changed the running PTY")
		}
		connection.Close()
		connection = connect()
		readPrefixedTerminalMarker(t, connection)
	}
	do("POST", "api/auth/logout", struct{}{}, nil)
	awaitRevocation()
}

func readPrefixedTerminalMarker(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	must(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	var output strings.Builder
	for !strings.Contains(output.String(), "PREFIX_TERMINAL_OK") {
		var event struct {
			Type, Data, Error string
			Binary            bool
		}
		must(t, connection.ReadJSON(&event))
		if event.Type == "error" {
			t.Fatal(event.Error)
		}
		if event.Type == "data" {
			data := []byte(event.Data)
			if event.Binary {
				decoded, err := base64.StdEncoding.DecodeString(event.Data)
				data = decoded
				must(t, err)
			}
			output.Write(data)
		}
	}
}
