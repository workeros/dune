package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	localidentity "github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/upgradecontrol"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/transport/ws"
	"github.com/aiomni/dune/pkg/upgrade"
	"github.com/fasthttp/websocket"
)

// This test installs unique user service jobs and cleans them up. It runs real
// release executables and the independent worker, not service command shims.
func TestNativeRunnerOnlineUpgrade(t *testing.T) {
	testNativeRunnerUpgrade(t, false)
}

func TestNativeRunnerUpgradePreview(t *testing.T) {
	testNativeRunnerUpgrade(t, true)
}

func testNativeRunnerUpgrade(t *testing.T, previewOnly bool) {
	if os.Getenv("DUNE_TEST_SERVICE_MANAGER") != "1" {
		t.Skip("set DUNE_TEST_SERVICE_MANAGER=1 for native installation and upgrade acceptance")
	}
	tmuxPath, err := filepath.Abs("../../bin/tmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUNE_TMUX", tmuxPath)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	if err := service.CheckManager(ctx); err != nil {
		t.Skip("native user service manager unavailable")
	}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	check(err)
	root, stateDir := filepath.Join(base, "installation"), filepath.Join(base, "state")
	check(os.MkdirAll(root, 0700))
	check(os.MkdirAll(stateDir, 0700))
	artifacts := filepath.Join(base, "artifacts")
	if volume := os.Getenv("DUNE_TEST_RELEASE_SOURCE_DIR"); volume != "" {
		artifacts, err = os.MkdirTemp(volume, "dune-native-release-")
		check(err)
		defer os.RemoveAll(artifacts)
		left, err := os.Stat(artifacts)
		check(err)
		right, err := os.Stat(root)
		check(err)
		if left.Sys().(*syscall.Stat_t).Dev == right.Sys().(*syscall.Stat_t).Dev {
			t.Fatal("native source and installation must be on distinct filesystems")
		}
		t.Logf("native source and installation filesystem devices %d / %d", left.Sys().(*syscall.Stat_t).Dev, right.Sys().(*syscall.Stat_t).Dev)
	}
	check(os.MkdirAll(artifacts, 0700))
	archiveServer := httptest.NewServer(http.FileServer(http.Dir(artifacts)))
	defer archiveServer.Close()
	manifests := map[string]upgrade.Manifest{}
	entryOverlay := nativePendingHostOverlay(t)
	for _, version := range []string{"v1", "v2", "v3", "v3-notices"} {
		dir := filepath.Join(artifacts, version)
		check(os.MkdirAll(filepath.Join(dir, "licenses"), 0700))
		arguments := []string{"build", "-ldflags", "-X github.com/aiomni/dune/internal/buildinfo.Version=" + version, "-o", filepath.Join(dir, "dune")}
		if version == "v1" {
			arguments = append(arguments, "-overlay", entryOverlay)
		}
		build := exec.CommandContext(ctx, "go", append(arguments, "../../cmd/dune")...)
		if version == "v3-notices" {
			body, err := os.ReadFile(filepath.Join(artifacts, "v3", "dune"))
			check(err)
			check(os.WriteFile(filepath.Join(dir, "dune"), body, 0700))
		} else if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v %s", version, err, output)
		}
		for _, name := range []string{"tmux", "rg"} {
			body, err := os.ReadFile(filepath.Join("../../bin", name))
			check(err)
			check(os.WriteFile(filepath.Join(dir, name), body, 0700))
		}
		check(os.WriteFile(filepath.Join(dir, "licenses", "NOTICE"), []byte("local acceptance fixture "+version), 0600))
		m, err := release.LocalManifest(ctx, dir, version, statecontract.ID(), upgrade.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH})
		check(err)
		archivePath := filepath.Join(artifacts, version+".tar.gz")
		archive, err := os.Create(archivePath)
		check(err)
		compressed := gzip.NewWriter(archive)
		writer := tar.NewWriter(compressed)
		for _, c := range m.Components {
			check(writer.WriteHeader(&tar.Header{Name: c.Path, Mode: int64(c.Mode), Size: c.Bytes, Typeflag: tar.TypeReg}))
			file, err := os.Open(filepath.Join(dir, c.Path))
			check(err)
			_, err = io.Copy(writer, file)
			check(err)
			check(file.Close())
		}
		check(writer.Close())
		check(compressed.Close())
		check(archive.Close())
		body, err := os.ReadFile(archivePath)
		check(err)
		digest := sha256.Sum256(body)
		m.ArchiveURL, m.ArchiveSHA256 = archiveServer.URL+"/"+version+".tar.gz", hex.EncodeToString(digest[:])
		manifests[version] = m
	}
	var app *App
	var cluster *nativeUpgradeCluster
	var controlCluster atomic.Pointer[nativeUpgradeCluster]
	var currentApp atomic.Pointer[App]
	var rejectV3 atomic.Bool
	var rejected atomic.Int32
	var holdProof atomic.Bool
	var rejectProof atomic.Bool
	confirmationRejected := make(chan upgrade.Probe, 1)
	proofReached := make(chan upgrade.Probe, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serving := currentApp.Load()
		if serving == nil {
			http.Error(w, "host restarting", 503)
			return
		}
		if r.URL.Path == "/api/v1/runner-upgrade-control" && (holdProof.Load() || rejectProof.Load()) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request upgradecontrol.Request
			if json.Unmarshal(body, &request) == nil && request.Action == "confirm" {
				// Suspend only after a real routed proof exists. A first confirm
				// request can precede connector startup and is not this boundary.
				_, err := serving.verifyUpgradeRoute(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), request.Probe)
				if err != nil {
					serving.ServeHTTP(w, r)
					return
				}
				if rejectProof.Load() {
					observed, err := installation.View(r.Context(), root)
					if err == nil && observed.Release.ID == "v3-notices" {
						select {
						case confirmationRejected <- request.Probe:
						default:
						}
						writeUpgradeControl(w, http.StatusServiceUnavailable, upgradecontrol.Response{ErrorCode: "PLATFORM_CONFIRMATION_TIMEOUT"})
						return
					}
				}
				if holdProof.Load() {
					select {
					case proofReached <- request.Probe:
					default:
					}
				}
				for holdProof.Load() {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
		}
		if remote := controlCluster.Load(); remote != nil && r.URL.Path == "/api/v1/runner-upgrade-control" {
			remote.proxy.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/connector/api/v1/ws/tunnel" {
			serving.ServeHTTP(w, r)
			return
		}
		grant, handler, err := serving.authorizer.Authorize(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		online := grant.Online
		grant.Online = func(ctx context.Context, binding api.Binding) error {
			observed, err := installation.View(ctx, root)
			if err == nil && rejectV3.Load() && observed.Release.ID == "v3" {
				rejected.Add(1)
				return fmt.Errorf("acceptance fixture rejects target route registration")
			}
			if online != nil {
				return online(ctx, binding)
			}
			return nil
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = serving.core.ServeConn(ctx, ws.NetConn(connection), grant, handler)
	}))
	defer server.Close()
	options := Options{DataDir: filepath.Join(base, "metadata"), PublicURL: server.URL, UpgradeSource: releaseSourceFunc(func(ctx context.Context, ref upgrade.ReleaseRef, p upgrade.Platform) (upgrade.Manifest, error) {
		m := manifests[ref.ID]
		if !m.Matches(ref) || m.Platform != p {
			return upgrade.Manifest{}, fmt.Errorf("unpublished release")
		}
		return m, nil
	})}
	cluster = newNativeUpgradeCluster(t, ctx, &options, manifests)
	controlCluster.Store(cluster)
	app, err = Open(ctx, options)
	check(err)
	if cluster != nil {
		cluster.serveEntry(t, app)
	}
	currentApp.Store(app)
	defer func() { app.Close() }()
	principal := identity.User{ID: "native-upgrade-user"}
	cookieHash := sha256.Sum256([]byte(nativeUpgradeCookie))
	check(app.store.RegisterAccount(ctx, localidentity.Account{User: principal, Salt: "unused", PasswordHash: "unused"}, hex.EncodeToString(cookieHash[:]), time.Now().Add(time.Hour).Unix()))
	logical := runner.Runner{ID: "native-runner", Name: "Native upgrade Runner", Kind: "managed"}
	token, _, err := app.store.IssueManagedEnrollment(ctx, principal, principal.ID, logical, "test-fabric")
	check(err)
	machine, credential, err := app.store.Enroll(ctx, token, runtime.GOOS, runtime.GOARCH)
	check(err)
	binding := runner.Binding{RunnerID: logical.ID, FabricID: "test-fabric", MachineID: machine.ID, Revision: 1}
	scope := upgrade.Scope{Principal: principal, OwnerID: principal.ID, Binding: binding}
	configPath := filepath.Join(base, "config.yaml")
	configuration := []byte(fmt.Sprintf("gateway: %s/connector/api/v1/ws/tunnel\nupgrade_control_url: %s/api/v1/runner-upgrade-control\ntoken: %s\ntarget: %s\nsession_dir: %s\n", strings.Replace(server.URL, "http", "ws", 1), server.URL, credential, machine.ID, stateDir))
	check(os.WriteFile(configPath, configuration, 0600))
	name := "dune-online-test-" + wire.ID()
	home, err := os.UserHomeDir()
	check(err)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, job := range []string{name, name + "-upgrade"} {
			if runtime.GOOS == "darwin" {
				_ = exec.CommandContext(cleanup, "launchctl", "bootout", fmt.Sprintf("gui/%d/com.dune.%s", os.Getuid(), job)).Run()
				_ = os.Remove(filepath.Join(home, "Library/LaunchAgents", "com.dune."+job+".plist"))
			} else {
				_ = exec.CommandContext(cleanup, "systemctl", "--user", "disable", "--now", job+".service").Run()
				_ = os.Remove(filepath.Join(home, ".config/systemd/user", job+".service"))
			}
		}
		for _, dir := range []string{stateDir, filepath.Join(stateDir, "acp")} {
			if server, err := tmux.Open(dir); err == nil {
				_ = server.Close()
			}
		}
		if t.Failed() {
			_ = filepath.Walk(filepath.Join(root, "diagnostics"), func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					body, _ := os.ReadFile(path)
					t.Log(string(body))
				}
				return nil
			})
		}
	}()
	install := exec.CommandContext(ctx, filepath.Join(artifacts, "v1", "dune"), "--config", configPath, "install", "--root", root, "--name", name, "--method", "managed")
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install: %v %s", err, output)
	}
	apiService := app.RunnerUpgrader()
	if cluster != nil {
		apiService = cluster.service
	}
	wait := func(f func() bool) {
		t.Helper()
		for !f() {
			select {
			case <-ctx.Done():
				t.Fatal("native upgrade deadline exceeded")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	var current upgrade.Inspection
	wait(func() bool {
		var err error
		current, err = apiService.InspectRunner(ctx, scope)
		return err == nil && current.Running.Build.Version == "v1"
	})
	t.Logf("installed actual v1 process %d", current.Running.PID)
	if !current.RunningFromSelectedRelease {
		t.Fatal("standard service does not execute the selected release")
	}
	initialManifest := manifests["v1"]
	initialDigest, err := initialManifest.Digest()
	check(err)
	noChange := upgrade.Request{SubmissionID: "already-current", Binding: binding, InstallationID: current.Installation.ID, ExpectedInstallationRevision: "999", ExpectedRunningSHA256: current.Running.SHA256, Release: upgrade.ReleaseRef{ID: initialManifest.ID, ManifestSHA256: initialDigest}}
	unchanged, err := apiService.StartUpgrade(ctx, scope, noChange)
	check(err)
	if unchanged.Operation.Phase != upgrade.AlreadyCurrent || !unchanged.Operation.Confirmed {
		t.Fatal("complete selected release was not recognized", unchanged)
	}
	repeated, err := apiService.StartUpgrade(ctx, scope, noChange)
	check(err)
	if repeated.Operation.ID != unchanged.Operation.ID {
		t.Fatal("no-op duplicate created another operation")
	}
	afterNoChange, err := apiService.InspectRunner(ctx, scope)
	check(err)
	if afterNoChange.Running.StartID != current.Running.StartID || afterNoChange.Installation.Revision != current.Installation.Revision {
		t.Fatal("no-op changed the installation or restarted the connector")
	}
	mock := filepath.Join(base, "mock-acp")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", mock, "../../samples/mock-acp").CombinedOutput(); err != nil {
		t.Fatalf("mock Agent: %v %s", err, output)
	}
	work := filepath.Join(base, "work")
	check(os.Mkdir(work, 0700))
	rpcLog := filepath.Join(base, "rpc.log")
	var agent *agentConnection
	connectAgent := func() {
		t.Helper()
		connection, err := app.AgentExecutor().Open(ctx, AgentScope{Principal: principal, OwnerID: principal.ID, Binding: binding})
		check(err)
		agent = connection.(*agentConnection)
	}
	connectAgent()
	defer func() { agent.Close() }()
	profile := api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}, Env: map[string]string{"DUNE_MOCK_RPC_LOG": rpcLog}}
	lateHosts := []*nativePendingHost{
		startNativePendingHost(t, ctx, agent, profile, filepath.Join(base, "late-rollback-rpc.log")),
		startNativePendingHost(t, ctx, agent, profile, filepath.Join(base, "late-success-rpc.log")),
	}
	rt, err := agent.Start(ctx, wire.ID(), profile)
	check(err)
	wait(func() bool { state, err := agent.State(ctx, rt); return err == nil && state.Ready })
	opened, err := agent.Submit(ctx, rt, AgentAction{SubmissionID: wire.ID(), Action: "new", Cwd: work})
	check(err)
	opened, err = agent.WaitOperation(ctx, rt, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 5000})
	check(err)
	if opened.State != "completed" {
		t.Fatal(opened)
	}
	originalHost, err := agent.Get(ctx, rt)
	check(err)
	if originalHost.ACPHost == nil || originalHost.ACPHost.Build.Version != "v1" {
		t.Fatal("v1 retained host missing", originalHost)
	}
	previewTarget := manifests["v2"]
	previewDigest, err := previewTarget.Digest()
	check(err)
	preview, err := apiService.PreviewUpgrade(ctx, scope, upgrade.PreviewRequest{Binding: binding, Release: upgrade.ReleaseRef{ID: previewTarget.ID, ManifestSHA256: previewDigest}})
	check(err)
	if !preview.Allowed || !preview.Plan.ReleaseUpdateRequired || !preview.Plan.ConnectorRestartRequired || len(preview.SourceCheck.Hosts) != 3 || len(preview.TargetCheck.Hosts) != 3 {
		t.Fatal("preview omitted current or pending host obligations", preview)
	}
	afterPreview, err := apiService.InspectRunner(ctx, scope)
	check(err)
	seal, err := launchgate.ReadSeal(stateDir)
	check(err)
	history, err := apiService.ListUpgrades(ctx, scope, upgrade.ListRequest{Binding: binding, InstallationID: current.Installation.ID})
	check(err)
	if afterPreview.Installation.Revision != current.Installation.Revision || afterPreview.Running.StartID != current.Running.StartID || seal != nil || history.Page.Active != nil || len(history.Page.Items) != 1 {
		t.Fatal("preview changed installation, service, seal or task history", afterPreview, history)
	}
	t.Log("public preview checked source, candidate and three retained/admitted hosts without changing installation or task history")
	if previewOnly {
		return
	}
	target := agent.target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = rt.ID, rt.Incarnation, rt.Generation
	control := func(action api.ACPAction) {
		t.Helper()
		_, err := agent.sdk.ACPControl(ctx, api.SubmissionKey{SubmissionID: wire.ID(), Target: target}, action)
		check(err)
	}
	ptyProfile := api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: work, Start: api.Command{Argv: []string{"/bin/sh", "-c", "printf '%s\n' \"$$\" > terminal.pid; exec /bin/sh"}}}
	ptyStart, ptyStream, err := agent.sdk.Start(ctx, api.StartRequest{SubmissionKey: api.SubmissionKey{SubmissionID: wire.ID(), Target: agent.target}, Profile: ptyProfile})
	check(err)
	if ptyStream != nil {
		ptyStream.Close()
	}
	if ptyStart.Runtime == nil {
		t.Fatal("PTY Runtime not started", ptyStart)
	}
	terminal := *ptyStart.Runtime
	var terminalPID int
	wait(func() bool {
		body, err := os.ReadFile(filepath.Join(work, "terminal.pid"))
		if err != nil {
			return false
		}
		terminalPID, err = strconv.Atoi(strings.TrimSpace(string(body)))
		return err == nil && terminalPID > 1
	})
	var previous []upgrade.Operation

	for _, attempt := range []struct {
		version string
		failure string
		crash   bool
	}{{"v2", "", false}, {"v3", "registration", false}, {"v3", "", false}, {"v3-notices", "confirmation", false}, {"v3-notices", "", true}} {
		pending, err := agent.Submit(ctx, rt, AgentAction{SubmissionID: wire.ID(), Action: "prompt", Text: "permission across upgrade", ExpectedConversationID: opened.ConversationID})
		check(err)
		wait(func() bool {
			state, err := agent.sdk.ACPState(ctx, rt)
			return err == nil && len(state.Permissions) == 1
		})
		agent.Close()
		rejectV3.Store(attempt.failure == "registration")
		rejectProof.Store(attempt.failure == "confirmation")
		m := manifests[attempt.version]
		digest, err := m.Digest()
		check(err)
		current, err = apiService.InspectRunner(ctx, scope)
		check(err)
		request := upgrade.Request{SubmissionID: wire.ID(), Binding: binding, InstallationID: current.Installation.ID, ExpectedInstallationRevision: current.Installation.Revision, ExpectedRunningSHA256: current.Running.SHA256, Release: upgrade.ReleaseRef{ID: m.ID, ManifestSHA256: digest}}
		holdProof.Store(attempt.crash)
		started, err := apiService.StartUpgrade(ctx, scope, request)
		check(err)
		if started.Operation.Admission != api.SubmissionAccepted {
			t.Fatal("upgrade was not admitted", started)
		}
		var writtenDuringFailure string
		if attempt.failure == "confirmation" {
			select {
			case proof := <-confirmationRejected:
				if proof.OperationID != started.Operation.ID {
					t.Fatal("wrong confirmation rejected")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			connectAgent()
			state, err := agent.sdk.ACPState(ctx, rt)
			check(err)
			if len(state.Permissions) != 1 {
				t.Fatal("original in-flight permission unavailable under seal")
			}
			control(api.ACPAction{Action: "permission", PermissionID: state.Permissions[0].ID, OptionID: "allow"})
			finished, err := agent.WaitOperation(ctx, rt, api.AgentOperationWait{Ref: pending.Ref, TimeoutMS: 5000})
			check(err)
			if finished.State != "completed" {
				t.Fatal(finished)
			}
			writtenDuringFailure = finished.Ref
			pending, err = agent.Submit(ctx, rt, AgentAction{SubmissionID: wire.ID(), Action: "prompt", Text: "permission recorded by v1 under v3", ExpectedConversationID: opened.ConversationID})
			check(err)
			wait(func() bool {
				state, err := agent.sdk.ACPState(ctx, rt)
				return err == nil && len(state.Permissions) == 1
			})
			agent.Close()
		}
		var survivingTargetPID int
		if attempt.crash {
			select {
			case proof := <-proofReached:
				if proof.OperationID != started.Operation.ID {
					t.Fatal("wrong attempt reached checkpoint")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			workerPID := nativeUpgradeServicePID(t, ctx, name+"-upgrade")
			check(syscall.Kill(workerPID, syscall.SIGKILL))
			check(service.Run(ctx, "stop", name+"-upgrade"))
			var lock *installation.Store
			wait(func() bool { var err error; lock, err = installation.Lock(root); return err == nil })
			// Holding the released installation lock deliberately postpones recovery.
			seal, err := launchgate.ReadSeal(stateDir)
			check(err)
			if seal == nil || seal.OperationID != started.Operation.ID {
				lock.Close()
				t.Fatal("worker death lost persistent seal")
			}
			gate, err := launchgate.Acquire(stateDir, false)
			if err == nil {
				gate.Close()
				lock.Close()
				t.Fatal("released worker lock reopened launch admission")
			}
			connectAgent()
			_, err = agent.Start(ctx, "rejected-during-worker-gap", profile)
			if err == nil || !strings.Contains(err.Error(), "UPGRADE_IN_PROGRESS") {
				lock.Close()
				t.Fatal("new launch was not sealed", err)
			}
			receipt, err := agent.sdk.QuerySubmission(ctx, api.SubmissionKey{SubmissionID: "rejected-during-worker-gap", Target: agent.target})
			check(err)
			if receipt.Admission != api.SubmissionNotAccepted {
				lock.Close()
				t.Fatal(receipt)
			}
			inspected, err := apiService.InspectRunner(ctx, scope)
			check(err)
			survivingTargetPID = inspected.Running.PID
			agent.Close()
			currentApp.Store(nil)
			check(app.Close())
			if cluster != nil {
				cluster.kill(t)
			}
			app, err = Open(ctx, options)
			check(err)
			if cluster != nil {
				cluster.serveEntry(t, app)
				cluster.start(t, ctx)
			}
			currentApp.Store(app)
			apiService = app.RunnerUpgrader()
			if cluster != nil {
				apiService = cluster.service
			}
			cached, err := apiService.GetUpgrade(ctx, scope, upgrade.Query{Binding: binding, InstallationID: request.InstallationID, SubmissionID: request.SubmissionID})
			check(err)
			if cached.Operation.ID != started.Operation.ID || cached.Operation.Confirmed {
				lock.Close()
				t.Fatal("host reopen lost original unconfirmed submission", cached)
			}
			check(lock.Close())
			metadata, err := installation.MetadataAt(root)
			check(err)
			check(service.Install(ctx, service.Installation{Root: root, ConfigPath: configPath, Name: name, PATH: metadata.ServicePATH}))
			holdProof.Store(false)
			t.Logf("killed actual worker %d after target %d routed; launches remained durably sealed", workerPID, survivingTargetPID)
		}
		query := upgrade.Query{Binding: binding, InstallationID: request.InstallationID, SubmissionID: request.SubmissionID}
		var result upgrade.Observation
		wait(func() bool {
			var err error
			result, err = apiService.GetUpgrade(ctx, scope, query)
			return err == nil && result.Operation.Confirmed && !result.Operation.LaunchSealed
		})
		op := result.Operation
		if attempt.failure != "" {
			if op.Phase != upgrade.Failed || op.Rollback != upgrade.RollbackRestored || rejected.Load() == 0 {
				t.Fatalf("target registration refusal did not roll back: %+v", op)
			}
		} else if op.Phase != upgrade.Succeeded {
			body, _ := json.Marshal(op)
			t.Fatalf("target did not succeed: %s", body)
		}
		if op.Proof == nil || !op.Proof.Routed || !op.Proof.GatewayAccepted || op.Proof.Running.PID == current.Running.PID {
			t.Fatal("missing actual replacement and routed proof", op)
		}
		if survivingTargetPID != 0 && op.Proof.Running.PID != survivingTargetPID {
			t.Fatal("recovery restarted already verified target")
		}
		connectAgent()
		for index, late := range lateHosts {
			found := false
			for _, participant := range op.Participants {
				if participant.Runtime.ID == late.runtime.ID && participant.ProgramSHA256 == initialManifest.ProgramSHA256() && participant.StateContract == statecontract.ID() {
					found = true
				}
			}
			if !found {
				t.Fatal("upgrade omitted admitted v1 host", late.runtime.ID, op.Participants)
			}
			if !late.entered && ((index == 0 && attempt.failure == "registration") || (index == 1 && attempt.version == "v3" && attempt.failure == "")) {
				late.resume(t)
			}
			late.verify(t, ctx, agent)
		}
		if attempt.crash {
			_, err := agent.Start(ctx, "rejected-during-worker-gap", profile)
			if err == nil || !strings.Contains(err.Error(), "UPGRADE_IN_PROGRESS") {
				t.Fatal("old rejected key executed after recovery", err)
			}
		}
		if writtenDuringFailure != "" {
			finished, err := agent.WaitOperation(ctx, rt, api.AgentOperationWait{Ref: writtenDuringFailure, TimeoutMS: 1000})
			check(err)
			if finished.State != "completed" || op.Failure == nil || op.Failure.Code != "PLATFORM_CONFIRMATION_TIMEOUT" {
				t.Fatal("rollback lost post-switch records or cause", finished, op.Failure)
			}
		}
		pty, err := agent.sdk.Get(ctx, terminal)
		check(err)
		if pty.ID != terminal.ID || pty.Incarnation != terminal.Incarnation || pty.Generation != terminal.Generation || pty.State != "running" || syscall.Kill(terminalPID, 0) != nil {
			t.Fatal("upgrade replaced original PTY process", pty, terminalPID)
		}
		retained, err := agent.Get(ctx, rt)
		check(err)
		if retained.ACPHost == nil || retained.ACPHost.HostPID != originalHost.ACPHost.HostPID || retained.ACPHost.AgentPID != originalHost.ACPHost.AgentPID || retained.ACPHost.Build.Version != "v1" {
			t.Fatal("upgrade replaced retained v1 host", retained)
		}
		state, err := agent.sdk.ACPState(ctx, rt)
		check(err)
		if len(state.Permissions) != 1 {
			t.Fatal("inflight permission lost", state)
		}
		control(api.ACPAction{Action: "permission", PermissionID: state.Permissions[0].ID, OptionID: "allow"})
		finished, err := agent.WaitOperation(ctx, rt, api.AgentOperationWait{Ref: pending.Ref, TimeoutMS: 5000})
		check(err)
		if finished.State != "completed" {
			t.Fatal("retained prompt did not finish", finished)
		}
		cancelled, err := agent.Submit(ctx, rt, AgentAction{SubmissionID: wire.ID(), Action: "prompt", Text: "permission to cancel", ExpectedConversationID: opened.ConversationID})
		check(err)
		wait(func() bool {
			state, err := agent.sdk.ACPState(ctx, rt)
			return err == nil && len(state.Permissions) == 1
		})
		control(api.ACPAction{Action: "cancel", OperationRef: cancelled.Ref})
		finished, err = agent.WaitOperation(ctx, rt, api.AgentOperationWait{Ref: cancelled.Ref, TimeoutMS: 5000})
		check(err)
		if !finished.Terminal() {
			t.Fatal("retained cancellation did not complete", finished)
		}
		previous = append(previous, op)
		for _, historical := range previous {
			observation, err := apiService.GetUpgrade(ctx, scope, upgrade.Query{Binding: binding, InstallationID: historical.Request.InstallationID, OperationID: historical.ID})
			check(err)
			if observation.Operation.Phase != historical.Phase || observation.Operation.Proof.Running.StartID != historical.Proof.Running.StartID {
				t.Fatal("later upgrade rewrote history")
			}
		}
		actual, err := os.ReadFile(configPath)
		check(err)
		if string(actual) != string(configuration) {
			t.Fatal("configuration changed")
		}
		t.Logf("%s rollback=%t phase=%s rollback_state=%s actual PID %d→%d rejected_routes=%d", attempt.version, attempt.failure != "", op.Phase, op.Rollback, current.Running.PID, op.Proof.Running.PID, rejected.Load())
	}
	body, err := os.ReadFile(rpcLog)
	check(err)
	methods := strings.Fields(string(body))
	counts := map[string]int{}
	for _, method := range methods {
		counts[method]++
	}
	if counts["initialize"] != 1 || counts["session/new"] != 1 || counts["session/prompt"] != 11 || counts["session/cancel"] != 5 {
		t.Fatal("Agent RPC replay or loss", counts)
	}
	_, err = agent.Stop(ctx, terminal, wire.ID())
	check(err)
	_, err = agent.Stop(ctx, rt, wire.ID())
	check(err)
	for _, late := range lateHosts {
		_, err := agent.Stop(ctx, late.runtime, wire.ID())
		check(err)
	}
	t.Logf("retained v1 host=%d Agent=%d; native RPC counts=%v", originalHost.ACPHost.HostPID, originalHost.ACPHost.AgentPID, counts)

}

func nativeUpgradeServicePID(t *testing.T, ctx context.Context, name string) int {
	t.Helper()
	var output []byte
	var err error
	if runtime.GOOS == "darwin" {
		output, err = exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/com.dune.%s", os.Getuid(), name)).Output()
		if err != nil {
			t.Fatal("read isolated service PID", err)
		}
		for _, line := range strings.Split(string(output), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
			if ok && key == "pid" {
				pid, err := strconv.Atoi(value)
				if err == nil && pid > 1 {
					return pid
				}
			}
		}
	} else {
		output, err = exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=MainPID", "--value", name+".service").Output()
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
			if err == nil && pid > 1 {
				return pid
			}
		}
	}
	t.Fatal("isolated worker PID unavailable", err)
	return 0
}
