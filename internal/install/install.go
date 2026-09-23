// Package install creates the current standard installation baseline. All
// subsequent distribution changes belong to the durable online upgrade worker.
package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/internal/runningprogram"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/upgrade"
)

func Run(ctx context.Context, configPath, root, name, method string) error {
	if method != "service" && method != "managed" {
		return fmt.Errorf("installation method must be service or managed")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.UpgradeControlURL == "" {
		return fmt.Errorf("standard installation requires the original host upgrade endpoint")
	}
	if _, err := cfg.TLS(); err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := launchgate.CheckDirectory(root); err != nil {
		return err
	}
	installed, err := installation.Lock(root)
	if err != nil {
		return err
	}
	defer installed.Close()
	if _, err := os.Lstat(filepath.Join(root, "installation.json")); !os.IsNotExist(err) {
		return fmt.Errorf("installation already exists; use its upgrade API or recovery command")
	}
	if _, err := os.Lstat(filepath.Join(root, "current")); !os.IsNotExist(err) {
		return fmt.Errorf("unrecognized installation layout; explicit prototype baseline recreation required")
	}
	if err := os.MkdirAll(cfg.SessionDir, 0700); err != nil {
		return err
	}
	for _, directory := range []string{"releases", "recovery"} {
		path := filepath.Join(root, directory)
		if _, err := os.Lstat(path); err == nil {
			if err := launchgate.CheckDirectory(path); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	metadata := installation.Metadata{ID: wire.ID(), Method: method, ConfigPath: configPath, StateDir: cfg.SessionDir, ServiceName: name}
	if err := metadata.Validate(root); err != nil {
		return err
	}
	for _, path := range []string{cfg.Certificate, cfg.Key} {
		if path == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(filepath.Join(root, "releases"), resolved)
		if err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return fmt.Errorf("trust material must remain outside releases")
		}
	}
	gate, report := fabricd.PrepareUpgrade(ctx, cfg.SessionDir)
	if gate == nil {
		return fmt.Errorf("standard installation preflight refused: %v", report.Issues)
	}
	defer gate.Close()
	if err := service.CheckManager(ctx); err != nil {
		return err
	}
	image, err := runningprogram.Inspect(ctx)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	source := filepath.Dir(executable)
	paths := []string{filepath.Join(root, "current")}
	for _, path := range filepath.SplitList(os.Getenv("PATH")) {
		if path == source || path == filepath.Join(root, "current") || strings.HasPrefix(path, filepath.Join(root, "releases")+string(filepath.Separator)) {
			continue
		}
		paths = append(paths, path)
	}
	metadata.ServicePATH = strings.Join(paths, string(os.PathListSeparator))
	manifest, err := release.LocalManifest(ctx, source, "local-"+image.SHA256[:16], statecontract.ID(), upgrade.Platform{OS: image.Build.OS, Arch: image.Build.Arch})
	if err != nil {
		return err
	}
	if manifest.ProgramSHA256() != image.SHA256 {
		return fmt.Errorf("local distribution differs from executing installer")
	}
	releases := filepath.Join(root, "releases")
	if err := os.MkdirAll(releases, 0700); err != nil {
		return err
	}
	if err := launchgate.CheckDirectory(releases); err != nil {
		return err
	}
	location := installation.Location{Directory: "releases/" + wire.ID(), Manifest: manifest}
	if err := release.StageLocal(ctx, source, filepath.Join(root, location.Directory), manifest); err != nil {
		return err
	}
	if err := release.StageLocal(ctx, source, filepath.Join(root, "recovery"), manifest); err != nil {
		return err
	}
	if err := os.Symlink(location.Directory, filepath.Join(root, "current")); err != nil {
		return err
	}
	if _, err := installed.Initialize(ctx, metadata, location); err != nil {
		return err
	}
	if err := installed.Register(); err != nil {
		return err
	}
	if err := service.Install(ctx, service.Installation{Root: root, ConfigPath: configPath, Name: name, PATH: metadata.ServicePATH}); err != nil {
		return err
	}
	fmt.Println("Dune installation recorded; connector and independent recovery service started. Check Runner status through the host.")
	return nil
}

// RepairServices restores only the two registered service jobs. It does not
// switch a distribution, re-enroll, clear a seal, or admit another upgrade.
func RepairServices(ctx context.Context, root string) error {
	installed, err := installation.Lock(root)
	if err != nil {
		return err
	}
	defer installed.Close()
	state, err := installed.Read()
	if err != nil {
		return err
	}
	return service.Install(ctx, service.Installation{Root: root, ConfigPath: state.Metadata.ConfigPath, Name: state.Metadata.ServiceName, PATH: state.Metadata.ServicePATH})
}
