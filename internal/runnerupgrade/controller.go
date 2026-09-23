package runnerupgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/internal/upgradecontrol"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

// Controller handles bounded connector requests. It never runs an accepted
// job: the separately supervised worker discovers durable admission itself.
type Controller struct {
	registration installation.Registration
	metadata     installation.Metadata
}

func ControllerFor(stateDir string) (*Controller, error) {
	registration, err := installation.Find(stateDir)
	if err != nil {
		return nil, issue("UPGRADE_UNSUPPORTED")
	}
	metadata, err := installation.MetadataAt(registration.Root)
	if err != nil {
		return nil, issue("INSTALLATION_UNVERIFIABLE")
	}
	return &Controller{registration: registration, metadata: metadata}, nil
}
func (c *Controller) control(binding runner.Binding) (*upgradecontrol.Client, error) {
	cfg, err := config.Load(c.metadata.ConfigPath)
	if err != nil || cfg.SessionDir != c.metadata.StateDir || cfg.Target != binding.MachineID {
		return nil, issue("UPGRADE_CONFIG_UNVERIFIABLE")
	}
	trust, err := cfg.TLS()
	if err != nil {
		return nil, issue("UPGRADE_CONFIG_UNVERIFIABLE")
	}
	client, err := upgradecontrol.New(cfg.UpgradeControlURL, cfg.Token, trust)
	if err != nil {
		return nil, issue("UPGRADE_UNSUPPORTED")
	}
	return client, nil
}

func (c *Controller) Start(ctx context.Context, request upgrade.Request, running api.RunningProgram) (upgrade.Operation, error) {
	unknown := upgrade.Operation{Request: request, Admission: api.SubmissionUnknown}
	if request.Validate() != nil {
		return unknown, issue("INVALID_ARGUMENT")
	}
	if request.InstallationID != c.registration.ID {
		return unknown, issue("INSTALLATION_CHANGED")
	}
	control, err := c.control(request.Binding)
	if err != nil {
		return unknown, err
	}
	defer control.Close()
	if _, err := control.Binding(ctx, request.Binding); err != nil {
		return unknown, err
	}
	jobs, err := upgradejob.Open(ctx, filepath.Join(c.registration.Root, "upgrades"))
	if err != nil {
		return unknown, issue("UPGRADE_STORE_UNAVAILABLE")
	}
	defer jobs.Close()
	if op, exists, err := jobs.Existing(ctx, request); err != nil || exists {
		return op, err
	}
	reject := func(cause error) (upgrade.Operation, error) {
		return jobs.Reject(ctx, request, failureIssue(cause, "admission").Code)
	}
	active, err := jobs.Active(ctx)
	if err != nil {
		return unknown, issue("UPGRADE_STORE_UNAVAILABLE")
	}
	if active != nil {
		return reject(issue("UPGRADE_CONFLICT"))
	}
	seal, err := launchgate.ReadSeal(c.metadata.StateDir)
	if err != nil || seal != nil {
		return reject(issue("UPGRADE_RECOVERY_REQUIRED"))
	}
	manifest, err := control.Resolve(ctx, request.Binding, request.Release, upgrade.Platform{OS: running.Build.OS, Arch: running.Build.Arch})
	if err != nil {
		return reject(err)
	}
	installed, err := installation.Lock(c.registration.Root)
	if errors.Is(err, installation.ErrBusy) {
		active, err := jobs.Active(ctx)
		if err != nil {
			return unknown, issue("UPGRADE_STORE_UNAVAILABLE")
		}
		if active != nil {
			return reject(issue("UPGRADE_CONFLICT"))
		}
		return reject(issue("INSTALLATION_BUSY"))
	}
	if err != nil {
		return reject(issue("INSTALLATION_UNVERIFIABLE"))
	}
	defer installed.Close()
	state, err := installed.Read()
	if err != nil || state.Metadata != c.metadata {
		return reject(issue("INSTALLATION_CHANGED"))
	}
	observed, err := installed.Observe(ctx)
	if err != nil {
		return reject(issue("INSTALLATION_UNVERIFIABLE"))
	}
	configurationSHA, err := configurationFingerprint(c.metadata.ConfigPath)
	if err != nil {
		return reject(err)
	}
	if _, err := control.Binding(ctx, request.Binding); err != nil {
		return reject(err)
	}
	source := upgrade.Inspection{Binding: request.Binding, Installation: &observed, Running: running, Supported: true, Issues: []upgrade.Issue{}}
	return jobs.Admit(ctx, request, manifest, source, configurationSHA)
}

func (c *Controller) Preview(ctx context.Context, request upgrade.PreviewRequest, source upgrade.Inspection) (upgrade.Preview, error) {
	result := upgrade.Preview{Source: source, Issues: []upgrade.Issue{}}
	if !request.Binding.Valid() || request.Binding != source.Binding || source.Installation == nil || source.Installation.ID != c.registration.ID {
		return result, issue("INVALID_ARGUMENT")
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	control, err := c.control(request.Binding)
	if err != nil {
		return result, err
	}
	defer control.Close()
	manifest, err := control.Resolve(bounded, request.Binding, request.Release, upgrade.Platform{OS: source.Running.Build.OS, Arch: source.Running.Build.Arch})
	if err != nil {
		return result, err
	}
	result.Target = manifest
	result.Plan = upgrade.Compare(*source.Installation, source.Running, manifest)
	// One ephemeral directory owns the download and extracted candidate. It is
	// outside releases and never selected or registered as an installation.
	cache, err := os.MkdirTemp("", "dune-upgrade-preview-")
	if err != nil {
		return result, issue("UPGRADE_CACHE_UNAVAILABLE")
	}
	defer os.RemoveAll(cache)
	candidate := filepath.Join(cache, "candidate")
	if err := release.Download(bounded, nil, manifest, candidate); err != nil {
		return result, issue("RELEASE_DOWNLOAD_FAILED")
	}
	installed, err := installation.Lock(c.registration.Root)
	if err != nil {
		return result, issue("INSTALLATION_BUSY")
	}
	current, err := installed.Read()
	observed, observationErr := installed.Observe(ctx)
	installed.Close()
	if observationErr != nil || observed.Revision != source.Installation.Revision {
		return result, issue("INSTALLATION_CHANGED")
	}
	if err != nil {
		return result, issue("INSTALLATION_UNVERIFIABLE")
	}
	result.SourceCheck, err = checkProgram(bounded, filepath.Join(c.registration.Root, current.Current.Directory, "dune"), c.metadata.ConfigPath, current.Current.Manifest)
	if err != nil {
		result.Issues = append(result.Issues, failureIssue(err, "source_check"))
	}
	result.TargetCheck, err = checkProgram(bounded, filepath.Join(candidate, "dune"), c.metadata.ConfigPath, manifest)
	if err != nil {
		result.Issues = append(result.Issues, failureIssue(err, "target_check"))
	}
	result.Allowed = len(result.Issues) == 0 && result.SourceCheck.Allowed && result.TargetCheck.Allowed
	return result, nil
}

func (c *Controller) Get(ctx context.Context, query upgrade.Query) (upgrade.Operation, error) {
	if query.InstallationID != c.registration.ID {
		return upgrade.Operation{}, issue("INSTALLATION_CHANGED")
	}
	jobs, err := upgradejob.OpenReadOnly(ctx, filepath.Join(c.registration.Root, "upgrades"))
	if err != nil {
		if os.IsNotExist(err) {
			return upgrade.Operation{Request: upgrade.Request{Binding: query.Binding, SubmissionID: query.SubmissionID, InstallationID: query.InstallationID}, Admission: api.SubmissionUnknown}, issue("UPGRADE_NOT_FOUND")
		}
		return upgrade.Operation{}, issue("UPGRADE_STORE_UNAVAILABLE")
	}
	defer jobs.Close()
	return jobs.Get(ctx, query)
}
func (c *Controller) List(ctx context.Context, request upgrade.ListRequest) (upgrade.Page, error) {
	if request.InstallationID != c.registration.ID {
		return upgrade.Page{}, issue("INSTALLATION_CHANGED")
	}
	jobs, err := upgradejob.OpenReadOnly(ctx, filepath.Join(c.registration.Root, "upgrades"))
	if err != nil {
		if os.IsNotExist(err) {
			return upgrade.Page{Items: []upgrade.Operation{}}, nil
		}
		return upgrade.Page{}, issue("UPGRADE_STORE_UNAVAILABLE")
	}
	defer jobs.Close()
	return jobs.List(ctx, request.Binding, request.InstallationID, request.Cursor, request.Limit)
}
