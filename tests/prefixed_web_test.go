package tests

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/transport/ws"
	"github.com/fasthttp/websocket"
)

func TestPrefixedWorkbenchEnrollmentAndTerminal(t *testing.T) {
	t.Run("default", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{}) })
	t.Run("machine-entry-override", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{override: true}) })
	t.Run("external-host", func(t *testing.T) { testPrefixedWorkbench(t, workbenchCase{external: true}) })
}

type workbenchCase struct {
	override, external, separateGateway bool
	humanCLI                            bool
	database                            *storage.Config
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
	if mode.database != nil {
		options.DataDir, options.Database = "", mode.database
	}
	if mode.separateGateway {
		if mode.database == nil {
			t.Fatal("separate gateway requires PostgreSQL")
		}
		remote := httptest.NewUnstartedServer(nil)
		remoteSite := "http://" + remote.Listener.Addr().String() + "/tools/dune/"
		onlineSite = remoteSite
		remoteApp, err := host.Open(ctx, host.Options{PublicURL: remoteSite, Database: mode.database})
		must(t, err)
		defer remoteApp.Close()
		remote.Config.Handler = remoteApp
		remote.Start()
		defer remote.Close()
		options.GatewayURL = "ws" + strings.TrimPrefix(remoteSite, "http") + "tunnel"
		options.DialGateway = func(ctx context.Context, token string) (net.Conn, error) {
			return ws.Dial(ctx, options.GatewayURL, token, &tls.Config{MinVersion: tls.VersionTLS12})
		}
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
	do("POST", "api/auth/register", map[string]string{"email": "prefix@example.test", "password": "prefix-test-password"}, &user)
	var enrollment struct{ Token string }
	do("POST", "api/enrollments", map[string]string{"name": "prefixed machine"}, &enrollment)
	machinePath := filepath.Join(dir, "machine", "config.yaml")
	must(t, webapp.EnrollMachine(ctx, machinePath, site, enrollment.Token, ""))
	machineConfig, err := config.Load(machinePath)
	must(t, err)
	expectedGateway := options.GatewayURL
	if expectedGateway == "" {
		expectedGateway = "ws" + strings.TrimPrefix(site, "http") + "tunnel"
	}
	if machineConfig.Gateway != expectedGateway {
		t.Fatalf("machine address = %s", machineConfig.Gateway)
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
		var machines []struct {
			ID     string
			Online bool
		}
		// The directory is still local at S1. In the separate-Gateway case,
		// wait for fabricd on its actual owner; execution below goes through A.
		response, err := browser.Get(onlineSite + "api/machines")
		must(t, err)
		if response.StatusCode != 200 {
			response.Body.Close()
			t.Fatalf("machine readiness: %d", response.StatusCode)
		}
		err = json.NewDecoder(response.Body).Decode(&machines)
		response.Body.Close()
		must(t, err)
		if len(machines) == 1 && machines[0].Online {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("prefixed machine tunnel did not connect")
		case <-time.After(25 * time.Millisecond):
		}
	}
	var runtime api.Runtime
	do("POST", "api/machines/"+machineConfig.Target+"/sessions", profile(dir, "pty", "/bin/sh"), &runtime)
	events := site + "api/machines/" + machineConfig.Target + "/sessions/" + runtime.ID + "/events"
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
	if app != nil {
		started := time.Now()
		must(t, app.SetPrincipalEnabled(ctx, user.ID, false))
		awaitRevocation()
		t.Logf("principal suspension closed idle terminal in %s", time.Since(started))
		for _, reenable := range []bool{false, true} {
			if reenable {
				must(t, app.SetPrincipalEnabled(ctx, user.ID, true))
			}
			response, err := browser.Get(site + "api/me")
			must(t, err)
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatal("suspended session revived")
			}
		}
		do("POST", "api/auth/login", map[string]string{"email": "prefix@example.test", "password": "prefix-test-password"}, nil)
		var remaining []api.Runtime
		do("POST", "api/machines/"+machineConfig.Target+"/call", map[string]any{"operation": "runtime.list", "payload": struct{}{}}, &remaining)
		if len(remaining) != 1 || remaining[0].ID != runtime.ID || remaining[0].Incarnation != runtime.Incarnation || remaining[0].Generation != runtime.Generation || remaining[0].State != "running" {
			t.Fatal("principal suspension changed the running PTY")
		}
		connection.Close()
		connection = connect()
		readPrefixedTerminalMarker(t, connection)
		_, err := app.LinkIdentity(ctx, identity.LinkRequest{RequestID: "terminal-link", Actor: "admin:regression", PrincipalID: user.ID, Namespace: "https://identity.example.test", Subject: "terminal-owner", Reason: "verified regression account ownership"})
		must(t, err)
		awaitRevocation()
		response, err := browser.Get(site + "api/me")
		must(t, err)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatal("pre-link session survived")
		}
		do("POST", "api/auth/login", map[string]string{"email": "prefix@example.test", "password": "prefix-test-password"}, nil)
		do("POST", "api/machines/"+machineConfig.Target+"/call", map[string]any{"operation": "runtime.list", "payload": struct{}{}}, &remaining)
		if len(remaining) != 1 || remaining[0].ID != runtime.ID || remaining[0].Incarnation != runtime.Incarnation || remaining[0].Generation != runtime.Generation || remaining[0].State != "running" {
			t.Fatal("identity link changed the running PTY")
		}
		connection.Close()
		connection = connect()
		readPrefixedTerminalMarker(t, connection)
	}
	var checkCLIRevoked func()
	if mode.humanCLI {
		checkCLIRevoked = exerciseHumanCLI(t, ctx, site, dir, machineConfig.Target, func(id, code string) {
			do("POST", "api/auth/cli/"+id, map[string]any{"code": code, "approve": true}, nil)
		})
	}
	do("POST", "api/auth/logout", struct{}{}, nil)
	awaitRevocation()
	if checkCLIRevoked != nil {
		checkCLIRevoked()
	}
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
