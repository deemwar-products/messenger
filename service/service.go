// Package service installs the messenger hub as a native OS service so ONE hub starts on
// boot and restarts on crash — launchd on macOS, systemd --user on Linux. It is only ever
// invoked EXPLICITLY (`messenger install --service` / `uninstall --service`), never
// silently. Secrets stay in the existing env file (sourced at launch); the unit file
// holds only NAMES and paths, never a value. No third-party deps.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const label = "com.deemwar.messenger"

// Config is what the service needs to launch the hub.
type Config struct {
	Bin     string // absolute path to the messenger binary
	Addr    string // serve address, e.g. ":14310"
	Home    string // $MESSENGER_HOME (not a secret)
	EnvFile string // file sourced for secret env vars (e.g. serve-token.env); "" = none
}

// launchCmd is the shell the service runs: source the (optional) secret env file, then
// exec the hub. Sourcing keeps secret VALUES out of the unit file — only the path shows.
func (c Config) launchCmd() string {
	var b strings.Builder
	if c.EnvFile != "" {
		fmt.Fprintf(&b, "[ -f %q ] && . %q; ", c.EnvFile, c.EnvFile)
	}
	fmt.Fprintf(&b, "exec %q serve --addr %q", c.Bin, c.Addr)
	return b.String()
}

// Install writes the platform service unit, enables it, and starts it.
func Install(c Config) error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(c)
	case "linux":
		return installSystemd(c)
	default:
		return fmt.Errorf("service: no built-in installer for %s — run `%s serve` under your process manager", runtime.GOOS, c.Bin)
	}
}

// Uninstall stops and removes the service.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	default:
		return fmt.Errorf("service: no built-in uninstaller for %s", runtime.GOOS)
	}
}

// Status returns a human-readable line about the service, or "" if not installed.
func Status() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat(launchdPlistPath()); err != nil {
			return "not installed (no launchd agent)", nil
		}
		out, _ := exec.Command("launchctl", "list").Output()
		if strings.Contains(string(out), label) {
			return "installed + loaded (launchd)", nil
		}
		return "installed but not loaded (launchd)", nil
	case "linux":
		if _, err := os.Stat(systemdUnitPath()); err != nil {
			return "not installed (no systemd user unit)", nil
		}
		out, _ := exec.Command("systemctl", "--user", "is-active", "messenger.service").Output()
		return "installed (systemd --user), is-active=" + strings.TrimSpace(string(out)), nil
	default:
		return "", fmt.Errorf("service: unsupported on %s", runtime.GOOS)
	}
}

// --- macOS launchd ---------------------------------------------------------------------

func launchdPlistPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", label+".plist")
}

func installLaunchd(c Config) error {
	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>%s</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>MESSENGER_HOME</key><string>%s</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, label, xmlEscape(c.launchCmd()), c.Home, filepath.Join(c.Home, "serve.log"), filepath.Join(c.Home, "serve.log"))
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	// Reload: unload any old copy, then load. Ignore unload error (may not be loaded).
	_ = exec.Command("launchctl", "unload", path).Run()
	if out, err := exec.Command("launchctl", "load", path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallLaunchd() error {
	path := launchdPlistPath()
	_ = exec.Command("launchctl", "unload", path).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// --- Linux systemd (user) --------------------------------------------------------------

func systemdUnitPath() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user", "messenger.service")
}

func installSystemd(c Config) error {
	path := systemdUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=messenger conversation hub (the ONE hub per host)
After=network-online.target

[Service]
Environment=MESSENGER_HOME=%s
ExecStart=/bin/sh -c '%s'
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, c.Home, c.launchCmd())
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "messenger.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user enable --now: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallSystemd() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", "messenger.service").Run()
	if err := os.Remove(systemdUnitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
