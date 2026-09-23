package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/runningprogram"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/internal/wire"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/internal/upgradecontrol"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

type releaseSourceFunc func(context.Context, upgrade.ReleaseRef, upgrade.Platform) (upgrade.Manifest, error)

func (f releaseSourceFunc) Resolve(ctx context.Context, ref upgrade.ReleaseRef, platform upgrade.Platform) (upgrade.Manifest, error) {
	return f(ctx, ref, platform)
}

func TestWorkerControlAuthenticatesOriginalMachineAndTrustedReleaseSource(t *testing.T) {
	f := openExecutorFixture(t)
	manifest := upgrade.Manifest{ID: "target", Platform: upgrade.Platform{OS: "linux", Arch: "amd64"}, ArchiveURL: "https://releases.example/dune.tar.gz", ArchiveSHA256: strings.Repeat("a", 64), StateContract: statecontract.ID()}
	for _, path := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		mode := uint32(0700)
		if strings.HasPrefix(path, "licenses/") {
			mode = 0600
		}
		manifest.Components = append(manifest.Components, upgrade.Component{Path: path, SHA256: strings.Repeat("b", 64), Bytes: 10, Mode: mode})
	}
	digest, err := manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ref := upgrade.ReleaseRef{ID: manifest.ID, ManifestSHA256: digest}
	var calls atomic.Int32
	f.app.upgradeSource = releaseSourceFunc(func(ctx context.Context, got upgrade.ReleaseRef, platform upgrade.Platform) (upgrade.Manifest, error) {
		calls.Add(1)
		if got != ref || platform != manifest.Platform {
			t.Error("source selection changed", got, platform)
		}
		return manifest, nil
	})
	server := httptest.NewServer(f.app)
	defer server.Close()
	control, err := upgradecontrol.New(server.URL+"/api/v1/runner-upgrade-control", f.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	observed, err := control.Binding(t.Context(), runner.Binding{})
	if err != nil || observed != f.binding {
		t.Fatal(observed, err)
	}
	selected, err := control.Resolve(t.Context(), f.binding, ref, manifest.Platform)
	if err != nil || !selected.Matches(ref) || calls.Load() != 1 {
		t.Fatal(selected, calls.Load(), err)
	}
	stale := f.binding
	stale.Revision++
	if _, err := control.Resolve(t.Context(), stale, ref, manifest.Platform); err == nil || calls.Load() != 1 {
		t.Fatal("stale binding reached release source", err)
	}
	unauthorized, err := upgradecontrol.New(server.URL+"/api/v1/runner-upgrade-control", strings.Repeat("0", 64), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unauthorized.Close()
	if _, err := unauthorized.Resolve(t.Context(), f.binding, ref, manifest.Platform); err == nil || calls.Load() != 1 {
		t.Fatal("unauthenticated source access", err)
	}
	// A live registered connector alone still cannot confirm an upgrade. This
	// fixture has no durable upgrade attempt or actual installation proof.
	probe := upgrade.Probe{Binding: f.binding, InstallationID: "installation", OperationID: "operation", AttemptID: "attempt", Challenge: "challenge"}
	proof, err := control.Confirm(t.Context(), probe)
	if err == nil || proof.GatewayAccepted || proof.Routed {
		t.Fatal("registration alone became upgrade confirmation", proof, err)
	}
}

func TestWorkerControlConfirmsActualImageThroughNormalGatewayRoute(t *testing.T) {
	f := openExecutorFixtureFor(t, time.Minute)
	ctx := t.Context()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	directory := "releases/" + wire.ID()
	program, err := runningprogram.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	manifest := upgrade.Manifest{ID: "current", Platform: upgrade.Platform{OS: program.Build.OS, Arch: program.Build.Arch}, ArchiveURL: "https://example.test/current.tar.gz", ArchiveSHA256: strings.Repeat("a", 64), StateContract: statecontract.ID()}
	for _, path := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		body, mode := []byte(path), os.FileMode(0700)
		if path == "dune" {
			body = image
		}
		if strings.HasPrefix(path, "licenses/") {
			mode = 0600
		}
		sum := sha256.Sum256(body)
		manifest.Components = append(manifest.Components, upgrade.Component{Path: path, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(body)), Mode: uint32(mode)})
		full := filepath.Join(root, directory, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	if manifest.ProgramSHA256() != program.SHA256 {
		t.Fatal("fixture differs from kernel image")
	}
	if err := os.Symlink(directory, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	installed, err := installation.Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer installed.Close()
	_, err = installed.Initialize(ctx, installation.Metadata{ID: "installation", Method: "managed", ConfigPath: configPath, StateDir: f.stateDir}, installation.Location{Directory: directory, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	if err := installed.Register(); err != nil {
		t.Fatal(err)
	}
	observed, err := installed.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := upgradejob.Open(ctx, filepath.Join(root, "upgrades"))
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	source := upgrade.Inspection{Binding: f.binding, Installation: &observed, Running: program, Supported: true}
	source.Running.SHA256, source.Running.StartID = strings.Repeat("b", 64), "previous-process"
	digest, _ := manifest.Digest()
	request := upgrade.Request{SubmissionID: "routed-proof", Binding: f.binding, InstallationID: observed.ID, ExpectedInstallationRevision: observed.Revision, ExpectedRunningSHA256: source.Running.SHA256, Release: upgrade.ReleaseRef{ID: manifest.ID, ManifestSHA256: digest}}
	op, err := jobs.Admit(ctx, request, manifest, source)
	if err != nil {
		t.Fatal(err)
	}
	record, err := jobs.Claim(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := launchgate.Acquire(f.stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	seal := launchgate.Seal{InstallationID: observed.ID, OperationID: op.ID, Owner: record.Owner}
	if err := gate.ClaimSeal(nil, seal); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []upgrade.Phase{upgrade.Preparing, upgrade.Downloading, upgrade.Checking, upgrade.Switching, upgrade.Verifying} {
		record, err = jobs.Update(ctx, op.ID, record.Owner, record.Operation.Revision, func(next *upgradejob.Record) error {
			next.Operation.Phase = phase
			if phase == upgrade.Switching {
				next.Operation.LaunchSealed = true
				next.Operation.AttemptID = "target-attempt"
				next.Operation.Challenge = "fresh-challenge"
				next.Operation.AttemptStartedAt = time.Now().UTC()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(f.app)
	defer server.Close()
	control, err := upgradecontrol.New(server.URL+"/api/v1/runner-upgrade-control", f.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	probe := upgrade.Probe{Binding: f.binding, InstallationID: observed.ID, OperationID: op.ID, AttemptID: record.Operation.AttemptID, Challenge: record.Operation.Challenge}
	proof, err := control.Confirm(ctx, probe)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.GatewayAccepted || !proof.Routed || !proof.ReleaseVerified || proof.Running.SHA256 != program.SHA256 || proof.Running.StartID != program.StartID {
		t.Fatalf("incomplete routed proof: %+v", proof)
	}
	still, err := jobs.Get(ctx, upgrade.Query{Binding: f.binding, InstallationID: observed.ID, OperationID: op.ID})
	if err != nil || still.Confirmed {
		t.Fatal("host proof became executor confirmation", still, err)
	}
	wrong := probe
	wrong.AttemptID = "late-attempt"
	if _, err := control.Confirm(ctx, wrong); err == nil {
		t.Fatal("accepted wrong attempt")
	}
	if err := os.Remove(filepath.Join(root, directory, "rg")); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Confirm(ctx, probe); err == nil {
		t.Fatal("confirmed incomplete distribution")
	}
	if err := os.WriteFile(filepath.Join(root, directory, "rg"), []byte("rg"), 0700); err != nil {
		t.Fatal(err)
	}
	proof, err = control.Confirm(ctx, probe)
	if err != nil {
		t.Fatal(err)
	}
	// The already received proof is only evidence. A durable executor transition
	// is the separate point at which success becomes externally observable.
	record, err = jobs.Update(ctx, op.ID, record.Owner, record.Operation.Revision, func(next *upgradejob.Record) error {
		next.Operation.Phase = upgrade.Succeeded
		next.Operation.Confirmed = true
		next.Operation.Proof = &proof
		return nil
	})
	if err != nil || !record.Operation.Confirmed {
		t.Fatal(record, err)
	}
}
