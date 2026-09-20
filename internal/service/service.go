// Package service installs a single per-user connector. Its private tmux server has
// an independent lifetime, so replacing/restarting fabricd preserves PTYs.
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
)

func Run(ctx context.Context, action, config, name, readyFile, readyNonce string) error {
	if (readyFile == "") != (readyNonce == "") {
		return fmt.Errorf("startup receipt path and nonce must be supplied together")
	}
	if name == "" || len(name) > 64 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return fmt.Errorf("service name must contain 1..64 letters, digits, - or _")
	}
	path, err := filepath.Abs(config)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	run := func(cmd string, args ...string) error {
		c := exec.CommandContext(ctx, cmd, args...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}
	switch runtime.GOOS {
	case "linux":
		unit := name + ".service"
		if action == "install" {
			dir := filepath.Join(home, ".config/systemd/user")
			if err = os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			quote := func(s string) string {
				return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
			}
			extra := ""
			if readyFile != "" {
				extra = " --ready-file " + quote(readyFile) + " --ready-nonce " + quote(readyNonce)
			}
			if _, statErr := os.Lstat(filepath.Join(dir, unit)); statErr == nil {
				if err = run("systemctl", "--user", "stop", unit); err != nil {
					return err
				}
			}
			body := "[Unit]\nDescription=Dune development machine connector\nAfter=network-online.target\n\n[Service]\nType=simple\nExecStart=" + quote(exe) + " --config " + quote(path) + " fabricd" + extra + "\nEnvironment=" + quote("PATH="+os.Getenv("PATH")) + "\nRestart=on-failure\nRestartSec=2\nKillMode=process\nTimeoutStopSec=15\n\n[Install]\nWantedBy=default.target\n"
			if err = os.WriteFile(filepath.Join(dir, unit), []byte(body), 0600); err != nil {
				return err
			}
			if err = run("systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
			if err = run("systemctl", "--user", "enable", unit); err != nil {
				return err
			}
			return run("systemctl", "--user", "restart", unit)
		}
		if action != "restart" && action != "stop" && action != "status" {
			return fmt.Errorf("service install|restart|stop|status")
		}
		return run("systemctl", "--user", action, unit)
	case "darwin":
		label := "com.dune." + name
		domain := "gui/" + strconv.Itoa(os.Getuid())
		plist := filepath.Join(home, "Library/LaunchAgents", label+".plist")
		if action == "install" {
			if err = os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
				return err
			}
			esc := func(s string) string { var b strings.Builder; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
			log := filepath.Join(filepath.Dir(path), name+".log")
			extra := ""
			if readyFile != "" {
				extra = `<string>--ready-file</string><string>` + esc(readyFile) + `</string><string>--ready-nonce</string><string>` + esc(readyNonce) + `</string>`
			}
			_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+label).Run()
			if err := ctx.Err(); err != nil {
				return err
			}
			body := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>` + esc(label) + `</string><key>ProgramArguments</key><array><string>` + esc(exe) + `</string><string>--config</string><string>` + esc(path) + `</string><string>fabricd</string>` + extra + `</array><key>EnvironmentVariables</key><dict><key>PATH</key><string>` + esc(os.Getenv("PATH")) + `</string></dict><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>AbandonProcessGroup</key><true/><key>StandardOutPath</key><string>` + esc(log) + `</string><key>StandardErrorPath</key><string>` + esc(log) + `</string></dict></plist>`
			if err = os.WriteFile(plist, []byte(body), 0600); err != nil {
				return err
			}
			return run("launchctl", "bootstrap", domain, plist)
		}
		switch action {
		case "restart":
			return run("launchctl", "kickstart", "-k", domain+"/"+label)
		case "stop":
			return run("launchctl", "bootout", domain+"/"+label)
		case "status":
			return run("launchctl", "print", domain+"/"+label)
		}
		return fmt.Errorf("service install|restart|stop|status")
	default:
		return fmt.Errorf("background service supports Linux and macOS")
	}
}
