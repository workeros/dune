// Package service owns per-user connector and upgrade-worker services. Their
// independent units/jobs have no shared stop or restart dependency.
package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/aiomni/dune/internal/launchgate"
)

type Installation struct{ Root, ConfigPath, Name, PATH string }
type definition struct {
	name        string
	program     string
	args        []string
	description string
}

func validName(name string) bool {
	return name != "" && len(name) <= 64 && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") == ""
}

// CheckManager is a precondition to standard installation, including bootstrap.
// No process-only fallback pretends to offer restart/reboot recovery.
func CheckManager(ctx context.Context) error {
	switch runtime.GOOS {
	case "linux":
		return command(ctx, "systemctl", "--user", "show-environment")
	case "darwin":
		return command(ctx, "launchctl", "print", domain())
	default:
		return fmt.Errorf("per-user service manager is unsupported on this platform")
	}
}

// Install creates two independent services. Both execute stable installation
// paths: the connector follows current; recovery keeps its own retained image.
func Install(ctx context.Context, install Installation) error {
	if !validName(install.Name) || !filepath.IsAbs(install.Root) || !filepath.IsAbs(install.ConfigPath) {
		return fmt.Errorf("standard service installation identity required")
	}
	if err := CheckManager(ctx); err != nil {
		return err
	}
	logDir := filepath.Join(install.Root, "diagnostics")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	if err := launchgate.CheckDirectory(logDir); err != nil {
		return err
	}
	definitions := []definition{
		{name: install.Name, program: filepath.Join(install.Root, "current", "dune"), args: []string{"--service-log-dir", logDir, "--config", install.ConfigPath, "fabricd"}, description: "Dune Runner connector"},
		{name: install.Name + "-upgrade", program: filepath.Join(install.Root, "recovery", "dune"), args: []string{"--config", install.ConfigPath, "upgrade-worker", "--root", install.Root}, description: "Dune Runner upgrade recovery"},
	}
	path := install.PATH
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, def := range definitions {
		if err := installDefinition(ctx, home, def, path); err != nil {
			return err
		}
	}
	return nil
}

func installDefinition(ctx context.Context, home string, def definition, path string) error {
	switch runtime.GOOS {
	case "linux":
		directory := filepath.Join(home, ".config/systemd/user")
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		unit := def.name + ".service"
		if err := writeDefinition(filepath.Join(directory, unit), systemdDefinition(def, path)); err != nil {
			return err
		}
		if err := command(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return command(ctx, "systemctl", "--user", "enable", "--now", unit)
	case "darwin":
		directory := filepath.Join(home, "Library/LaunchAgents")
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		file := filepath.Join(directory, "com.dune."+def.name+".plist")
		if err := writeDefinition(file, launchdDefinition(def, path)); err != nil {
			return err
		}
		if command(ctx, "launchctl", "print", domain()+"/com.dune."+def.name) == nil {
			return nil
		}
		return command(ctx, "launchctl", "bootstrap", domain(), file)
	default:
		return fmt.Errorf("unsupported service platform")
	}
}

func Run(ctx context.Context, action, name string) error {
	if !validName(name) {
		return fmt.Errorf("valid service name required")
	}
	switch runtime.GOOS {
	case "linux":
		if action != "restart" && action != "stop" && action != "status" {
			return fmt.Errorf("service restart|stop|status")
		}
		return command(ctx, "systemctl", "--user", action, name+".service")
	case "darwin":
		target := domain() + "/com.dune." + name
		switch action {
		case "restart":
			return command(ctx, "launchctl", "kickstart", "-k", target)
		case "stop":
			return command(ctx, "launchctl", "bootout", target)
		case "status":
			return command(ctx, "launchctl", "print", target)
		}
	}
	return fmt.Errorf("service restart|stop|status requires Linux or macOS")
}
func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func command(ctx context.Context, name string, args ...string) error {
	// Manager output may contain environment or full launch arguments. Public
	// diagnostics expose stage codes; never echo that output through an API.
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("service manager %s failed", name)
	}
	return nil
}
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}
func systemdDefinition(def definition, path string) string {
	args := []string{systemdQuote(def.program)}
	for _, arg := range def.args {
		args = append(args, systemdQuote(arg))
	}
	return "[Unit]\nDescription=" + def.description + "\nAfter=network-online.target\n\n[Service]\nType=simple\nExecStart=" + strings.Join(args, " ") + "\nEnvironment=" + systemdQuote("PATH="+path) + "\nRestart=always\nRestartSec=2\nKillMode=process\nTimeoutStopSec=15\nStandardOutput=null\nStandardError=null\n\n[Install]\nWantedBy=default.target\n"
}
func launchdDefinition(def definition, path string) string {
	esc := func(value string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(value))
		return b.String()
	}
	var args strings.Builder
	for _, arg := range append([]string{def.program}, def.args...) {
		args.WriteString("<string>" + esc(arg) + "</string>")
	}
	return `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>com.dune.` + esc(def.name) + `</string><key>ProgramArguments</key><array>` + args.String() + `</array><key>EnvironmentVariables</key><dict><key>PATH</key><string>` + esc(path) + `</string></dict><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>2</integer><key>AbandonProcessGroup</key><true/><key>StandardOutPath</key><string>/dev/null</string><key>StandardErrorPath</key><string>/dev/null</string></dict></plist>`
}

func writeDefinition(path, body string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
			return fmt.Errorf("unverifiable service definition")
		}
		existing, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(existing) != body {
			return fmt.Errorf("service belongs to another installation or was modified")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteString(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
