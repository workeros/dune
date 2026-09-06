package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
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
	"github.com/fasthttp/websocket"
)

func TestPrefixedWorkbenchEnrollmentAndTerminal(t *testing.T) {
	t.Run("default", func(t *testing.T) { testPrefixedWorkbench(t, false, false) })
	t.Run("machine-entry-override", func(t *testing.T) { testPrefixedWorkbench(t, true, false) })
	t.Run("external-host", func(t *testing.T) { testPrefixedWorkbench(t, false, true) })
}

func testPrefixedWorkbench(t *testing.T, override, external bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	site := origin + "/tools/dune/"
	var app *host.App
	options := host.Options{PublicURL: site, DataDir: filepath.Join(dir, "accounts")}
	if override {
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
	if external {
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
	do("POST", "api/auth/register", map[string]string{"email": "prefix@example.test", "password": "prefix-test-password"}, nil)
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
		do("GET", "api/machines", nil, &machines)
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
	defer connection.Close()
	must(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	must(t, connection.WriteJSON(map[string]string{"type": "input", "data": "printf 'PREFIX_TERMINAL_%s\\n' OK\n"}))
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
				data, err = base64.StdEncoding.DecodeString(event.Data)
				must(t, err)
			}
			output.Write(data)
		}
	}
	do("POST", "api/auth/logout", struct{}{}, nil)
	must(t, connection.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, _, err := connection.ReadMessage()
		if err != nil {
			if networkError, ok := err.(interface{ Timeout() bool }); ok && networkError.Timeout() {
				t.Fatal("logout did not revoke the idle prefixed terminal")
			}
			break
		}
	}
}
