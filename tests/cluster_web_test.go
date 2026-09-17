package tests

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

func TestPostgresClusterWebProcesses(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	dir := t.TempDir()
	writeYAML := func(path string, value any) {
		t.Helper()
		data, err := yaml.Marshal(value)
		must(t, err)
		must(t, os.WriteFile(path, data, 0600))
	}
	databaseFile := filepath.Join(dir, "database.yaml")
	writeYAML(databaseFile, map[string]any{"postgres": map[string]string{"url": postgresWorkbenchURL(t, database)}})
	ca := testcert.New(t)
	sites := make([]string, 3)
	processes := make([]*hostTestProcess, 3)
	for i := range sites {
		instanceDir := filepath.Join(dir, fmt.Sprint(i))
		must(t, os.Mkdir(instanceDir, 0700))
		public, err := net.Listen("tcp", "127.0.0.1:0")
		must(t, err)
		peer, err := net.Listen("tcp", "127.0.0.1:0")
		must(t, err)
		publicAddress, peerAddress := public.Addr().String(), peer.Addr().String()
		sites[i] = "http://" + publicAddress + "/dune/"
		cert := ca.Issue(t, "127.0.0.1", nil)
		key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		must(t, err)
		for name, data := range map[string][]byte{
			"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
			"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
			"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate.Raw}),
		} {
			must(t, os.WriteFile(filepath.Join(instanceDir, name), data, 0600))
		}
		clusterFile := filepath.Join(instanceDir, "cluster.yaml")
		writeYAML(clusterFile, map[string]any{"peer": map[string]string{"listen": peerAddress, "address": "https://" + peerAddress + "/api/v1/ws/peer", "certificate": "cert.pem", "key": "key.pem", "ca": "ca.pem"}})
		localFile := filepath.Join(instanceDir, "local.yaml")
		must(t, config.Create(localFile, config.Config{Gateway: "ws://" + publicAddress + "/dune/api/v1/ws/tunnel", Listen: publicAddress, Token: strings.Repeat("x", 32), Target: "unused-local-token"}))
		log, err := os.Create(filepath.Join(instanceDir, "web.log"))
		must(t, err)
		defer log.Close()
		must(t, public.Close())
		if i == 0 {
			// Peer remains occupied. Startup must fail and release the public
			// socket that it acquired before discovering this conflict.
			failed := exec.CommandContext(ctx, binary, "--config", localFile, "web", "--url", sites[i], "--database-config", databaseFile, "--cluster-config", clusterFile)
			output, err := failed.CombinedOutput()
			if err == nil || !strings.Contains(string(output), peerAddress) {
				t.Fatal("occupied peer port did not fail cluster startup")
			}
			released, err := net.Listen("tcp", publicAddress)
			must(t, err)
			must(t, released.Close())
			failed = exec.CommandContext(ctx, binary, "--config", localFile, "web", "--cluster-config", clusterFile)
			output, err = failed.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "requires a PostgreSQL") {
				t.Fatal("cluster Web process accepted the default SQLite backend")
			}
		}
		must(t, peer.Close())
		process := launchHostTestProcess(t, log, "--config", localFile, "web", "--url", sites[i], "--database-config", databaseFile, "--cluster-config", clusterFile)
		processes[i] = process
		defer process.stop(t, syscall.SIGTERM)
		t.Cleanup(func() {
			data, _ := os.ReadFile(log.Name())
			if strings.Contains(string(data), "DATA RACE") {
				t.Error("cluster process reported a data race")
			}
		})
		probe := &http.Client{Timeout: time.Second}
		for {
			response, err := probe.Get(sites[i] + "api/v1/bootstrap")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					break
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal("cluster Web process did not start")
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	jar, err := cookiejar.New(nil)
	must(t, err)
	browser := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	request := func(site, method, route string, body, out any) {
		t.Helper()
		data, err := json.Marshal(body)
		must(t, err)
		req, err := http.NewRequestWithContext(ctx, method, site+route, bytes.NewReader(data))
		must(t, err)
		origin, err := url.Parse(site)
		must(t, err)
		req.Header.Set("Origin", origin.Scheme+"://"+origin.Host)
		req.Header.Set("X-Dune-Request", "1")
		req.Header.Set("Content-Type", "application/json")
		response, err := browser.Do(req)
		must(t, err)
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 201 {
			t.Fatalf("cluster %s %s: %d", method, route, response.StatusCode)
		}
		if out != nil {
			must(t, json.NewDecoder(response.Body).Decode(out))
		}
	}
	credentials := map[string]string{"email": "cluster-web@example.test", "password": "cluster-process-password"}
	request(sites[0], "POST", "api/v1/auth/register", credentials, nil)
	var enrollment struct {
		Token  string
		Runner struct{ ID string }
	}
	request(sites[0], "POST", "api/v1/enrollments", map[string]string{"name": "cluster machine"}, &enrollment)
	machineFile := filepath.Join(dir, "machine", "config.yaml")
	// Enrollment itself is consumed on B after being issued by A.
	must(t, webapp.EnrollMachine(ctx, machineFile, sites[1], enrollment.Token, "", enrollment.Runner.ID))
	machine, err := config.Load(machineFile)
	must(t, err)
	log, err := os.Create(filepath.Join(dir, "fabricd.log"))
	must(t, err)
	defer log.Close()
	connector := launchHostTestProcess(t, log, "--config", machineFile, "fabricd")
	defer func() {
		connector.stop(t, syscall.SIGTERM)
		manager, err := tmux.Open(machine.SessionDir)
		if err == nil {
			manager.Close()
		}
	}()
	for _, entry := range []int{0, 2} {
		site := sites[entry]
		if entry != 0 {
			request(site, "POST", "api/v1/auth/login", credentials, nil)
		}
		for {
			var page struct {
				Items []struct {
					ID     string
					Online bool
				}
			}
			request(site, "GET", "api/v1/runners", nil, &page)
			if len(page.Items) == 1 && page.Items[0].ID == enrollment.Runner.ID && page.Items[0].Online {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("remote owner did not become visible")
			case <-time.After(25 * time.Millisecond):
			}
		}
		request(site, "POST", "api/v1/runners/"+enrollment.Runner.ID+"/call?fabric_id=attached&revision=1&machine_id="+machine.Target, map[string]any{"operation": "runtime.list", "payload": struct{}{}}, nil)
		// A different entry revokes the shared browser session.
		request(sites[1], "POST", "api/v1/auth/logout", struct{}{}, nil)
	}

	// Real Web processes share PostgreSQL routes; the connector belongs to B.
	// A Web submission on A and an MCP submission on C must reach one queue.
	request(sites[0], "POST", "api/v1/auth/login", credentials, nil)
	var principal identity.User
	request(sites[0], "GET", "api/v1/me", nil, &principal)
	query := "?fabric_id=attached&revision=1&machine_id=" + machine.Target
	runnerBase := "api/v1/runners/" + enrollment.Runner.ID
	var caller agents.LaunchResult
	callerProfile := profile(dir, "pty", "/bin/sh", "-c", "sleep 120")
	request(sites[0], "POST", runnerBase+"/sessions"+query, agents.StartRequest{Custom: &callerProfile}, &caller)
	store, err := metadata.Open(ctx, database)
	must(t, err)
	defer store.Close()
	// Fixture provisioning only: subsequent MCP authentication and every target
	// operation go through the real HTTP -> SDK -> peer Gateway -> fabricd path.
	token, err := store.IssueAgentCredential(ctx, agents.Scope{Principal: principal, OwnerID: principal.ID}, caller.Session.ID, caller.Session.Attempt.ID, time.Now().Add(time.Hour))
	must(t, err)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "cluster-test-agent", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: sites[2] + "api/v1/agent-mcp", HTTPClient: &http.Client{Transport: clusterMCPTransport{token: token}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	must(t, err)
	defer client.Close()
	callMCP := func(name string, input, out any) {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input})
		must(t, err)
		if result.IsError {
			t.Fatalf("cluster MCP %s failed", name)
		}
		data, err := json.Marshal(result.StructuredContent)
		must(t, err)
		must(t, json.Unmarshal(data, out))
	}
	mock := filepath.Join(dir, "mock-acp")
	output, err := exec.CommandContext(ctx, "go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock ACP build failed: %v %s", err, output)
	}
	agentProfile := profile(dir, "acp", mock)
	var target agents.LaunchResult
	request(sites[0], "POST", runnerBase+"/sessions"+query, agents.StartRequest{Custom: &agentProfile}, &target)
	var first, second agents.Operation
	request(sites[0], "POST", "api/v1/agents/prompt", agents.PromptRequest{AgentRef: target.AgentRef, Text: "permission for cluster A"}, &first)
	callMCP("agents_prompt", agents.PromptRequest{AgentRef: target.AgentRef, Text: "CLUSTER_SECOND_OPERATION"}, &second)
	if second.State != "pending" || first.Ref == second.Ref {
		t.Fatal("cross-host submissions did not share one serial queue")
	}
	var before agents.ReadResult
	callMCP("agents_read", agents.ReadRequest{OperationRef: second.Ref}, &before)
	if before.Operation == nil || before.Operation.State != "pending" || before.Operation.NextPosition != 0 || len(before.Operation.Output) != 0 {
		t.Fatal("queued B exposed A's output")
	}
	// The submitting Web process has no ownership of the accepted queue.
	processes[0].stop(t, syscall.SIGKILL)
	var waited agents.WaitResult
	callMCP("agents_wait", agents.WaitRequest{OperationRef: second.Ref, TimeoutMS: 50}, &waited)
	if waited.Operation == nil || waited.Operation.State != "pending" || !waited.TimedOut {
		t.Fatal("submitter exit changed the queued operation")
	}
	request(sites[1], "POST", "api/v1/agents/read", agents.ReadRequest{OperationRef: second.Ref}, &before)
	if before.Operation == nil || before.Operation.State != "pending" {
		t.Fatal("another host could not read pending")
	}
	restartWeb := func(index int) {
		t.Helper()
		old := processes[index]
		old.stop(t, syscall.SIGKILL)
		processes[index] = launchHostTestProcess(t, old.cmd.Stdout.(*os.File), old.cmd.Args[1:]...)
		for {
			response, err := browser.Get(sites[index] + "api/v1/bootstrap")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					break
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal("restarted Web process did not become ready")
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	restartWeb(0)
	request(sites[0], "POST", "api/v1/agents/wait", agents.WaitRequest{OperationRef: second.Ref, TimeoutMS: 50}, &waited)
	if waited.Operation == nil || waited.Operation.State != "pending" {
		t.Fatal("restarted host could not follow accepted operation")
	}
	var state struct {
		Permissions []struct {
			ID string `json:"id"`
		} `json:"permissions"`
	}
	request(sites[2], "POST", runnerBase+"/call"+query, map[string]any{"operation": "acp.state", "runtime": target.Runtime, "payload": struct{}{}}, &state)
	if len(state.Permissions) != 1 {
		t.Fatal("first operation did not hold its native permission")
	}
	request(sites[2], "POST", runnerBase+"/call"+query, map[string]any{"operation": "acp.action", "runtime": target.Runtime, "payload": api.ACPAction{Action: "permission", PermissionID: state.Permissions[0].ID, OptionID: "allow"}}, nil)
	for _, operation := range []agents.Operation{first, second} {
		callMCP("agents_wait", agents.WaitRequest{OperationRef: operation.Ref, TimeoutMS: 5000}, &waited)
		if waited.Operation == nil || waited.Operation.State != "completed" {
			t.Fatal("cross-host operation did not complete")
		}
	}
	var a, b agents.ReadResult
	request(sites[0], "POST", "api/v1/agents/read", agents.ReadRequest{OperationRef: first.Ref}, &a)
	callMCP("agents_read", agents.ReadRequest{OperationRef: second.Ref}, &b)
	if a.Operation == nil || b.Operation == nil || a.Operation.Incomplete || b.Operation.Incomplete || strings.Contains(string(api.Payload(a)), "CLUSTER_SECOND_OPERATION") || !strings.Contains(string(api.Payload(b)), "CLUSTER_SECOND_OPERATION") {
		t.Fatal("cross-host operation output boundaries changed")
	}
	// Read-only probes verify the full path. A database route can remain online
	// briefly after its owner dies, before its lease or connection is replaced.
	awaitCaller := func() {
		t.Helper()
		for {
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "agents_get", Arguments: map[string]string{"agent_ref": caller.AgentRef}})
			if err == nil && !result.IsError {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("MCP caller did not reconnect through Gateway")
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	// Restart the actual Gateway owner. fabricd remains alive and retains work.
	restartWeb(1)
	awaitCaller()
	var after agents.ReadResult
	callMCP("agents_read", agents.ReadRequest{OperationRef: second.Ref}, &after)
	if string(api.Payload(after)) != string(api.Payload(b)) {
		t.Fatal("Gateway owner restart lost fabricd output")
	}
	// A fabricd process restart is a different lifetime: ACP and its operations
	// disappear while the PTY caller remains valid. Old reads cannot fall back.
	connector.stop(t, syscall.SIGKILL)
	connector = launchHostTestProcess(t, log, "--config", machineFile, "fabricd")
	awaitCaller()
	expired, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "agents_read", Arguments: agents.ReadRequest{OperationRef: second.Ref}})
	must(t, err)
	if !expired.IsError {
		t.Fatal("fabricd restart returned output for an expired ACP operation")
	}
	var failure struct {
		Code string `json:"code"`
	}
	must(t, json.Unmarshal(api.Payload(expired.StructuredContent), &failure))
	if failure.Code != "STALE_RUNTIME" && failure.Code != "OPERATION_EXPIRED" {
		t.Fatal("old ACP reference did not report its lost execution lifetime", failure.Code)
	}
}

type clusterMCPTransport struct{ token string }

func (t clusterMCPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(request)
}
