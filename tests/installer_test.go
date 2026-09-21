package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/sdk"
)

// The fake commands only report to this test's HTTP server. Neither test invokes
// the machine's launchctl/systemctl or writes its real service directories.
const installServiceShim = `package main
import("bytes";"encoding/json";"fmt";"net/http";"os")
func main(){
 b,_:=json.Marshal(struct{Args,Env []string}{os.Args[1:],os.Environ()})
 r,e:=http.Post(os.Getenv("DUNE_INSTALL_TEST_MANAGER"),"application/json",bytes.NewReader(b))
 if e!=nil{fmt.Fprintln(os.Stderr,e);os.Exit(1)}
 defer r.Body.Close()
 if r.StatusCode!=200{fmt.Fprintln(os.Stderr,"private service manager rejected command");os.Exit(1)}
}`

type installStart struct{ args, env []string }

type installManager struct {
	t                *testing.T
	mu               sync.Mutex
	home, root, name string
	log              *os.File
	process          *hostTestProcess
	hold, wrongNonce bool
	pending          *installStart
	starts           chan installStart
	actions          []string
}

func (m *installManager) stop() {
	if m.process != nil {
		m.process.stop(m.t, syscall.SIGTERM)
		m.process = nil
	}
}

func (m *installManager) launch(start installStart) error {
	if len(start.args) < 4 || !strings.HasPrefix(start.args[0], m.root+string(filepath.Separator)) {
		return fmt.Errorf("service command escaped the private installation")
	}
	cmd := exec.Command(start.args[0], start.args[1:]...)
	cmd.Env = append(start.env, "TZ=UTC", "GORACE=atexit_sleep_ms=0")
	cmd.Stdout, cmd.Stderr = m.log, m.log
	if err := cmd.Start(); err != nil {
		return err
	}
	m.process = &hostTestProcess{cmd: cmd, done: make(chan error, 1)}
	process := m.process
	go func() { process.done <- cmd.Wait() }()
	return nil
}

func (m *installManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var request struct{ Args, Env []string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&request) != nil {
		http.Error(w, "invalid private service command", 400)
		return
	}
	args := request.Args
	if len(args) > 0 && args[0] == "--user" {
		args = args[1:]
	}
	if len(args) == 0 {
		http.Error(w, "missing action", 400)
		return
	}
	m.actions = append(m.actions, args[0])
	switch args[0] {
	case "stop", "bootout":
		m.stop()
	case "daemon-reload", "enable":
	case "bootstrap", "restart":
		m.stop()
		start := installStart{env: request.Env}
		var err error
		if runtime.GOOS == "darwin" {
			var data []byte
			data, err = os.ReadFile(filepath.Join(m.home, "Library/LaunchAgents/com.dune."+m.name+".plist"))
			if err == nil && (bytes.Count(data, []byte(`<string>/dev/null</string>`)) != 2 || !bytes.Contains(data, []byte(`<string>--service-log-dir</string>`))) {
				m.t.Error("service lacks bounded diagnostics or retains an unbounded output file")
			}
			var plist struct {
				Arguments []string `xml:"dict>array>string"`
				Values    []string `xml:"dict>dict>string"`
			}
			if err == nil {
				err = xml.Unmarshal(data, &plist)
			}
			start.args = plist.Arguments
			if len(plist.Values) == 1 {
				start.env = append(start.env, "PATH="+plist.Values[0])
			}
		} else {
			var data []byte
			data, err = os.ReadFile(filepath.Join(m.home, ".config/systemd/user", m.name+".service"))
			if err == nil && (!bytes.Contains(data, []byte("StandardOutput=null\nStandardError=null")) || !bytes.Contains(data, []byte("--service-log-dir"))) {
				m.t.Error("service lacks bounded diagnostics or retains unbounded output")
			}
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "ExecStart=") {
					for _, quoted := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"|[^\s"]+`).FindAllString(strings.TrimPrefix(line, "ExecStart="), -1) {
						value := quoted
						if strings.HasPrefix(quoted, `"`) {
							value, err = strconv.Unquote(quoted)
						}
						start.args = append(start.args, strings.ReplaceAll(value, "%%", "%"))
					}
				}
			}
		}
		if err == nil && m.wrongNonce {
			for i := range start.args {
				if start.args[i] == "--ready-nonce" && i+1 < len(start.args) {
					start.args[i+1] = wire.ID()
				}
			}
		}
		if err == nil {
			if m.hold {
				m.pending = &start
			} else {
				err = m.launch(start)
			}
		}
		m.starts <- start
		if err != nil {
			m.t.Log("private service start:", err)
			http.Error(w, err.Error(), 500)
		}
	default:
		http.Error(w, "unexpected service action", 400)
	}
}

type installerFixture struct {
	t                  *testing.T
	root, source, path string
	env                []string
	manager            *installManager
	ctx                context.Context
	cfg                config.Config
}

func newInstallerFixture(t *testing.T) *installerFixture {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("installer supports Linux and macOS")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	home, root := filepath.Join(dir, "home"), filepath.Join(dir, "installation")
	fakeBin, source := filepath.Join(dir, "private-bin"), filepath.Join(dir, "download")
	for _, path := range []string{home, root, fakeBin, source, filepath.Join(source, "licenses")} {
		must(t, os.MkdirAll(path, 0700))
	}
	for name, from := range map[string]string{"dune": binary, "tmux": "../bin/tmux", "rg": "../bin/rg"} {
		data, err := os.ReadFile(from)
		must(t, err)
		must(t, os.WriteFile(filepath.Join(source, name), data, 0700))
	}
	must(t, os.WriteFile(filepath.Join(source, "licenses", "NOTICE"), []byte("isolated installer fixture\n"), 0600))
	shimSource := filepath.Join(dir, "service-shim.go")
	must(t, os.WriteFile(shimSource, []byte(installServiceShim), 0600))
	shim := filepath.Join(fakeBin, "private-service-manager")
	build := exec.Command("go", "build", "-o", shim, shimSource)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build private service manager: %v\n%s", err, output)
	}
	for _, name := range []string{"launchctl", "systemctl"} {
		must(t, os.Symlink(shim, filepath.Join(fakeBin, name)))
	}
	log, err := os.Create(filepath.Join(dir, "services.log"))
	must(t, err)
	manager := &installManager{t: t, home: home, root: root, name: "installer-test", log: log, starts: make(chan installStart, 8)}
	server := httptest.NewServer(manager)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	fixture := &installerFixture{t: t, root: root, source: source, path: filepath.Join(dir, "config.yaml"), manager: manager, ctx: ctx}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "PATH", "DUNE_TMUX", "DUNE_RG", "GORACE", "TZ":
		default:
			fixture.env = append(fixture.env, entry)
		}
	}
	fixture.env = append(fixture.env, "HOME="+home, "PATH="+fakeBin+string(os.PathListSeparator)+source+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DUNE_INSTALL_TEST_MANAGER="+server.URL, "GORACE=atexit_sleep_ms=0", "TZ=Asia/Shanghai")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := listener.Addr().String()
	listener.Close()
	must(t, config.Init(fixture.path, address))
	fixture.cfg, err = config.Load(fixture.path)
	must(t, err)
	gateway := launchHostTestProcess(t, log, "--config", fixture.path, "gateway")
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.stop()
		manager.mu.Unlock()
		gateway.stop(t, syscall.SIGTERM)
		for _, directory := range []string{fixture.cfg.SessionDir, filepath.Join(fixture.cfg.SessionDir, "acp")} {
			if native, err := tmux.Open(directory); err == nil {
				_ = native.Close()
			}
		}
		server.Close()
		cancel()
		log.Close()
		data, _ := os.ReadFile(log.Name())
		if bytes.Contains(data, []byte("DATA RACE")) {
			t.Error("installer/fabricd data race")
		}
		if t.Failed() {
			t.Log(string(data))
		}
	})
	return fixture
}

func TestInstallerACPPreflightGateAndOriginalSessionAcrossSwitches(t *testing.T) {
	f := newInstallerFixture(t)
	stamp := func(version string) {
		t.Helper()
		build := exec.Command("go", "build", "-race", "-ldflags", "-X github.com/aiomni/dune/internal/buildinfo.Version="+version, "-o", filepath.Join(f.source, "dune"), "../cmd/dune")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	stamp("fixture-old")
	oldProgram, err := os.ReadFile(filepath.Join(f.source, "dune"))
	must(t, err)
	oldRelease := f.repair()
	client := f.dial()
	defer func() { client.Close() }()
	mock := mockACPBinary(t)
	work := t.TempDir()
	barrier, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer barrier.Close()
	p := profile(work, "acp", mock)
	p.ManagedACP = true
	p.Env = map[string]string{"DUNE_MOCK_PROCESS_LOG": filepath.Join(work, "agent.log"), "DUNE_MOCK_RPC_LOG": filepath.Join(work, "rpc.log"), "DUNE_MOCK_PROMPT_GATE": barrier.Addr().String()}
	rt, observer, err := testStartProfile(client, f.ctx, p)
	must(t, err)
	defer observer.Close()
	waitInstaller(t, func() bool { state, err := client.ACPState(f.ctx, rt); return err == nil && state.Ready })
	opened, err := testACPSubmit(client, f.ctx, rt, api.ACPAction{Action: "new"})
	must(t, err)
	opened, err = client.WaitAgentOperation(f.ctx, rt, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 5000})
	must(t, err)
	if opened.State != "completed" {
		t.Fatal(opened)
	}
	before, err := client.Get(f.ctx, rt)
	must(t, err)
	if before.ACPHost == nil {
		t.Fatal("missing host diagnostics")
	}
	identity := *before.ACPHost
	if identity.Build == nil || identity.Build.Version != "fixture-old" {
		t.Fatal("host lacks its actual embedded build", identity)
	}
	var machine api.MachineInfo
	must(t, client.Call(f.ctx, "machine.info", struct{}{}, &machine))
	if machine.Connector == nil || machine.Connector.Build.Version != "fixture-old" || machine.Connector.StartedAt.IsZero() {
		t.Fatal(machine)
	}
	var tmuxPID int
	for _, server := range machine.Tmux {
		if server.Namespace == "acp" && server.ServerState == "running" && server.ServerVersion == "3.7c" {
			tmuxPID = server.ServerPID
		}
	}
	if tmuxPID <= 1 {
		t.Fatal("actual ACP tmux version missing", machine.Tmux)
	}
	pinned := filepath.Join(f.cfg.SessionDir, "acp", "runtimes", rt.ID, "program")
	// The initial observer remains attached: only launch publication holds the
	// shared gate, not this long-lived stream.
	guard, report := fabricd.PrepareUpgrade(f.ctx, f.cfg.SessionDir)
	if guard == nil || !report.Allowed || len(report.Hosts) != 1 {
		t.Fatal(report)
	}
	defer guard.Close()
	target := api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: client.Binding.Target, BindingRevision: 1}
	key := api.SubmissionKey{SubmissionID: "launch-during-upgrade", Target: target}
	request := api.StartRequest{SubmissionKey: key, Profile: p}
	checkRejected := func() {
		t.Helper()
		result, stream, err := client.Start(f.ctx, request)
		if stream != nil {
			stream.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "UPGRADE_IN_PROGRESS") || result.SubmissionKey != key || result.Admission != api.SubmissionNotAccepted {
			t.Fatal("gate refusal did not preserve sealed key", result, err)
		}
		receipt, err := client.QuerySubmission(f.ctx, key)
		if err != nil || receipt.SubmissionKey != key || receipt.Admission != api.SubmissionNotAccepted {
			t.Fatal(receipt, err)
		}
	}
	checkRejected()
	// Current host operations do not take the gate or reset the Agent.
	prompt, err := testACPSubmit(client, f.ctx, rt, api.ACPAction{Action: "prompt", Text: "work during upgrade preflight", ExpectedConversationID: opened.ConversationID})
	must(t, err)
	prompt, err = client.WaitAgentOperation(f.ctx, rt, api.AgentOperationWait{Ref: prompt.Ref, TimeoutMS: 5000})
	must(t, err)
	if prompt.State != "completed" {
		t.Fatal(prompt)
	}
	guard.Close()
	checkRejected() // Even after the gate opens, this original intent stays sealed.
	assertRefusedBeforeService := func(code string) {
		t.Helper()
		f.manager.mu.Lock()
		count := len(f.manager.actions)
		pid := f.manager.process.cmd.Process.Pid
		f.manager.mu.Unlock()
		output, err := f.command("upgrade").CombinedOutput()
		if err == nil || !strings.Contains(string(output), code) || !strings.Contains(string(output), rt.ID) {
			t.Fatal("upgrade did not identify incompatible original Runtime", string(output), err)
		}
		f.manager.mu.Lock()
		unchanged := len(f.manager.actions) == count && f.manager.process.cmd.Process.Pid == pid
		f.manager.mu.Unlock()
		if !unchanged {
			t.Fatal("preflight failure invoked the service manager")
		}
		got, err := client.Get(f.ctx, rt)
		if err != nil || got.ACPHost == nil || got.ACPHost.AgentPID != identity.AgentPID || got.ACPHost.HostPID != identity.HostPID {
			t.Fatal("preflight failure changed original process", got, err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(f.cfg.SessionDir, "registry", "registry.sqlite"))
	must(t, err)
	defer db.Close()
	var registration []byte
	must(t, db.QueryRow(`SELECT registration FROM session_hosts WHERE instance=?`, identity.Instance).Scan(&registration))
	var fields map[string]json.RawMessage
	must(t, json.Unmarshal(registration, &fields))
	fields["version"] = json.RawMessage(`999`)
	_, err = db.Exec(`UPDATE session_hosts SET registration=? WHERE instance=?`, api.Payload(fields), identity.Instance)
	must(t, err)
	assertRefusedBeforeService("SESSION_PROTOCOL_UNSUPPORTED")
	_, err = db.Exec(`UPDATE session_hosts SET registration=? WHERE instance=?`, registration, identity.Instance)
	must(t, err)
	must(t, os.Rename(pinned, pinned+".held"))
	assertRefusedBeforeService("HOST_PROGRAM_UNAVAILABLE")
	must(t, os.Rename(pinned+".held", pinned))
	observer.Close()
	inflight, err := testACPSubmit(client, f.ctx, rt, api.ACPAction{Action: "prompt", Text: "barrier across upgrade and rollback", ExpectedConversationID: opened.ConversationID})
	must(t, err)
	must(t, barrier.(*net.TCPListener).SetDeadline(time.Now().Add(5*time.Second)))
	paused, err := barrier.Accept()
	must(t, err)
	defer paused.Close()
	var reached [1]byte
	_, err = io.ReadFull(paused, reached[:])
	must(t, err)
	// Distinct embedded build stamps and executable digests exercise mixed host
	// versions. Both are compiled from this source; historical release binaries
	// and different tmux versions still require their own acceptance run.
	var newer api.Runtime
	for _, version := range []string{"fixture-new", "fixture-old"} {
		if version == "fixture-new" {
			stamp(version)
		} else {
			// Publish another executable inode, as a downloaded release would.
			// Overwriting a previously executed Mach-O inode can invalidate macOS
			// cached code-signing pages and kill the rollback CLI before it runs.
			rollback := filepath.Join(f.source, "rollback-program")
			must(t, os.WriteFile(rollback, oldProgram, 0700))
			must(t, os.Rename(rollback, filepath.Join(f.source, "dune")))
		}
		client.Close()
		output, err := f.command("upgrade").CombinedOutput()
		if err != nil {
			t.Fatal(version, string(output), err)
		}
		<-f.manager.starts
		client = f.dial()
		must(t, client.Call(f.ctx, "machine.info", struct{}{}, &machine))
		if machine.Connector == nil || machine.Connector.Build.Version != version {
			t.Fatal("connector reports another build", version, machine)
		}
		for _, server := range machine.Tmux {
			if server.Namespace == "acp" && (server.ServerState != "running" || server.ServerPID != tmuxPID || server.ServerVersion != "3.7c") {
				t.Fatal("upgrade invented a new tmux server version", server)
			}
		}
		got, err := client.Get(f.ctx, rt)
		if err != nil || got.ACPHost == nil || got.ACPHost.Build == nil || got.ACPHost.Build.Version != "fixture-old" || got.ACPHost.HostPID != identity.HostPID || got.ACPHost.AgentPID != identity.AgentPID || got.ACPHost.ProgramSHA256 != identity.ProgramSHA256 || got.ACPHost.ConnectorTerm <= identity.ConnectorTerm {
			t.Fatal("service replacement lost original host", got, err)
		}
		retained, err := client.WaitAgentOperation(f.ctx, rt, api.AgentOperationWait{Ref: prompt.Ref, TimeoutMS: 1000})
		if err != nil || retained.State != "completed" || retained.Ref != prompt.Ref {
			t.Fatal("switch lost original operation", retained, err)
		}
		checkRejected()
		pending, err := client.WaitAgentOperation(f.ctx, rt, api.AgentOperationWait{Ref: inflight.Ref, TimeoutMS: 10})
		if err != nil || pending.State != "running" || pending.Ref != inflight.Ref {
			t.Fatal("switch lost admitted in-flight prompt", pending, err)
		}
		if version == "fixture-new" {
			profile := profile(t.TempDir(), "acp", mock)
			profile.ManagedACP = true
			var stream *sdk.Stream
			newer, stream, err = testStartProfile(client, f.ctx, profile)
			must(t, err)
			stream.Close()
		}
		newer, err = client.Get(f.ctx, newer)
		if err != nil || newer.ACPHost == nil || newer.ACPHost.Build == nil || newer.ACPHost.Build.Version != "fixture-new" || newer.ACPHost.ProgramSHA256 == identity.ProgramSHA256 {
			t.Fatal("new host did not retain its own build across rollback", newer, err)
		}
	}
	_, err = paused.Write([]byte{1})
	must(t, err)
	completed, err := client.WaitAgentOperation(f.ctx, rt, api.AgentOperationWait{Ref: inflight.Ref, TimeoutMS: 5000})
	if err != nil || completed.State != "completed" || completed.Ref != inflight.Ref {
		t.Fatal("original in-flight prompt did not complete", completed, err)
	}
	state, err := client.ACPState(f.ctx, rt)
	if err != nil || state.Conversation == nil || state.Conversation.ID != opened.ConversationID {
		t.Fatal("switch changed original conversation", state, err)
	}
	versionCommand := exec.CommandContext(f.ctx, filepath.Join(f.source, "dune"), "--config", f.path, "version", "--runner")
	versionCommand.Env = f.env
	versionJSON, err := versionCommand.Output()
	must(t, err)
	var versions struct {
		Executable api.BuildInfo   `json:"executable"`
		Machine    api.MachineInfo `json:"machine"`
		Runtimes   api.RuntimeList `json:"runtimes"`
	}
	must(t, json.Unmarshal(versionJSON, &versions))
	if versions.Executable.Version != "fixture-old" || versions.Machine.Connector == nil || versions.Machine.Connector.PID != machine.Connector.PID || len(versions.Runtimes.Items) != 2 || strings.Contains(string(versionJSON), f.cfg.Token) {
		t.Fatal("CLI diagnostics did not follow the original public boundary", versions)
	}
	if _, err := os.Stat(oldRelease); !os.IsNotExist(err) {
		t.Fatal("fixture did not remove original release", err)
	}
	processes, err := os.ReadFile(filepath.Join(work, "agent.log"))
	must(t, err)
	rpcs, err := os.ReadFile(filepath.Join(work, "rpc.log"))
	must(t, err)
	if strings.Count(string(processes), "\n") != 1 || string(rpcs) != "initialize\nsession/new\nsession/prompt\nsession/prompt\n" {
		t.Fatal("switch or gate rejection replayed Agent work", string(processes), string(rpcs))
	}
	guard, report = fabricd.PrepareUpgrade(f.ctx, f.cfg.SessionDir)
	if guard == nil {
		t.Fatal(report)
	}
	defer guard.Close()
	must(t, testStopRuntime(client, f.ctx, rt))
	must(t, testForgetRuntime(client, f.ctx, rt))
	must(t, testStopRuntime(client, f.ctx, newer))
	must(t, testForgetRuntime(client, f.ctx, newer))
	if _, err := os.Lstat(pinned); !os.IsNotExist(err) {
		t.Fatal("forget left pinned program", err)
	}
	t.Logf("two isolated service switches retained host=%d Agent=%d program=%s; no native service manager invoked", identity.HostPID, identity.AgentPID, identity.ProgramSHA256)
}

func (f *installerFixture) command(action string) *exec.Cmd {
	cmd := exec.CommandContext(f.ctx, filepath.Join(f.source, "dune"), "--config", f.path, action, "--root", f.root, "--name", f.manager.name)
	cmd.Env = f.env
	return cmd
}

func (f *installerFixture) repair() string {
	f.t.Helper()
	output, err := f.command("repair").CombinedOutput()
	if err != nil {
		f.t.Fatalf("repair: %v\n%s", err, output)
	}
	<-f.manager.starts
	current, err := filepath.EvalSymlinks(filepath.Join(f.root, "current"))
	must(f.t, err)
	return current
}

func (f *installerFixture) dial() *sdk.Client {
	f.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		client, err := sdk.Dial(f.ctx, sdk.Options{Gateway: f.cfg.Gateway, Token: f.cfg.Token, Target: f.cfg.Target})
		if err == nil {
			return client
		}
		time.Sleep(25 * time.Millisecond)
	}
	f.t.Fatal("installed fabricd did not connect")
	return nil
}

func waitInstaller(t *testing.T, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("installer state did not settle")
}

func TestInstallerReceiptGatesCleanupAndPTYTimeoutSurvivesUpgrade(t *testing.T) {
	f := newInstallerFixture(t)
	old := f.repair()
	client := f.dial()
	p := profile(filepath.Dir(f.path), "pty", "/bin/sh", "-c", `trap '' TERM; i=0; while [ "$i" -lt 80 ]; do echo "UPGRADE_HISTORY_$i"; i=$((i+1)); done; echo $$ > target-pid; exec sleep 60`)
	p.Start.TimeoutSeconds = 8
	rt, stream, err := testStartProfile(client, f.ctx, p)
	must(t, err)
	stream.Close()
	client.Close()
	if rt.DeadlineAt == nil {
		t.Fatal("timed Runtime lacks deadline")
	}
	var pid int
	waitInstaller(t, func() bool {
		data, _ := os.ReadFile(filepath.Join(filepath.Dir(f.path), "target-pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		return pid > 0
	})
	f.manager.mu.Lock()
	f.manager.hold = true
	f.manager.mu.Unlock()
	upgrade := f.command("upgrade")
	var output bytes.Buffer
	upgrade.Stdout, upgrade.Stderr = &output, &output
	must(t, upgrade.Start())
	done := make(chan error, 1)
	go func() { done <- upgrade.Wait() }()
	t.Cleanup(func() { _ = upgrade.Process.Kill() })
	select {
	case <-f.manager.starts:
	case <-f.ctx.Done():
		t.Fatal("upgrade did not request private service start")
	}
	// The fake service manager already accepted startup, but fabricd has not
	// initialized or written its receipt. Old programs must still be present.
	time.Sleep(250 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("upgrade finished without startup receipt: %v\n%s", err, output.String())
	default:
	}
	if _, err := os.Stat(filepath.Join(old, "dune")); err != nil {
		t.Fatal("old programs removed before receipt", err)
	}
	f.manager.mu.Lock()
	launchErr := f.manager.launch(*f.manager.pending)
	f.manager.pending = nil
	f.manager.hold = false
	f.manager.mu.Unlock()
	must(t, launchErr)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("upgrade: %v\n%s", err, output.String())
		}
	case <-f.ctx.Done():
		t.Fatal("upgrade did not finish")
	}
	current, err := filepath.EvalSymlinks(filepath.Join(f.root, "current"))
	must(t, err)
	if current == old {
		t.Fatal("upgrade did not switch directories")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("successful upgrade retained old programs", err)
	}
	client = f.dial()
	got, err := client.Get(f.ctx, rt)
	must(t, err)
	if got.State != "running" || got.DeadlineAt == nil || !got.DeadlineAt.Equal(*rt.DeadlineAt) {
		t.Fatal("upgrade lost the original running deadline", got)
	}
	client.Close()
	f.manager.mu.Lock()
	f.manager.stop()
	f.manager.mu.Unlock()
	// Both old mapped programs (tmux/helper) have been unlinked. No fabricd is
	// running while the original deadline expires.
	waitInstaller(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	f.manager.mu.Lock()
	launchErr = f.manager.launch(installStart{args: []string{filepath.Join(current, "dune"), "--config", f.path, "fabricd"}, env: f.env})
	f.manager.mu.Unlock()
	must(t, launchErr)
	client = f.dial()
	defer client.Close()
	got, err = client.Get(f.ctx, rt)
	must(t, err)
	if got.State != "exited" || got.StopReason != "timed_out" || got.DeadlineAt == nil || !got.DeadlineAt.Equal(*rt.DeadlineAt) {
		t.Fatal("new fabricd did not restore timed_out", got)
	}
	var capture api.TerminalSnapshot
	must(t, client.CallID(f.ctx, "runtime.capture", wire.ID(), struct{}{}, &capture, &rt))
	if capture.HistoryLines < 40 || !strings.Contains(capture.Content, "UPGRADE_HISTORY_79") {
		t.Fatal("upgrade or timeout destroyed history", capture)
	}
	must(t, testForgetRuntime(client, f.ctx, rt))
}

func TestInstallerRejectsWrongReceiptAndPreservesOldPrograms(t *testing.T) {
	f := newInstallerFixture(t)
	old := f.repair()
	f.manager.mu.Lock()
	f.manager.wrongNonce = true
	f.manager.mu.Unlock()
	upgrade := f.command("upgrade")
	var output bytes.Buffer
	upgrade.Stdout, upgrade.Stderr = &output, &output
	must(t, upgrade.Start())
	done := make(chan error, 1)
	go func() { done <- upgrade.Wait() }()
	t.Cleanup(func() { _ = upgrade.Process.Kill() })
	var start installStart
	select {
	case start = <-f.manager.starts:
	case <-f.ctx.Done():
		t.Fatal("upgrade did not request service start")
	}
	var receipt string
	for i := range start.args {
		if start.args[i] == "--ready-file" {
			receipt = start.args[i+1]
		}
	}
	waitInstaller(t, func() bool { _, err := os.Stat(receipt); return err == nil })
	data, err := os.ReadFile(receipt)
	must(t, err)
	var proof service.StartupReceipt
	must(t, json.Unmarshal(data, &proof))
	must(t, service.VerifyStartupReceipt(receipt, proof.Nonce, filepath.Dir(start.args[0]), time.Time{}))
	expected := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(receipt), ".startup-"), ".json")
	if err := service.VerifyStartupReceipt(receipt, expected, filepath.Dir(start.args[0]), time.Time{}); err == nil {
		t.Fatal("incorrect nonce accepted")
	}
	// Real fabricd initialized and produced a live-process receipt, but for a
	// different installation nonce. Cancel the pending install to bound the test.
	time.Sleep(200 * time.Millisecond)
	must(t, upgrade.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-done:
		if err == nil || !strings.Contains(output.String(), "did not confirm local initialization") {
			t.Fatalf("incorrect startup proof accepted: %v\n%s", err, output.String())
		}
	case <-f.ctx.Done():
		t.Fatal("cancelled upgrade did not finish")
	}
	current, err := filepath.EvalSymlinks(filepath.Join(f.root, "current"))
	must(t, err)
	if current != old {
		t.Fatal("failed upgrade changed current program directory")
	}
	if _, err := os.Stat(filepath.Join(old, "dune")); err != nil {
		t.Fatal("failed upgrade removed old programs", err)
	}
}
