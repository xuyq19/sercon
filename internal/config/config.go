// Package config loads the daemon configuration and picks sensible defaults.
//
// Every setting has a working default so that dropping the binary on a jump
// host and running it is enough to get going. The file only exists to pin down
// the things a lab actually cares about: baud rates per adapter, friendly
// names, and where logs land.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// PortDef pins settings for one port.
//
// On Windows this is where the COM-number weakness is papered over: a friendly
// name and a baud rate survive a replug even though COM3 may come back as COM5.
type PortDef struct {
	// Ref is the port reference, e.g. "COM3" or
	// "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0".
	Ref string `json:"ref"`
	// Desc is the label shown in listings.
	Desc string `json:"desc,omitempty"`
	// Baud overrides the global default for this port.
	Baud int `json:"baud,omitempty"`
}

// Config is the on-disk configuration.
type Config struct {
	// LogDir is the root for per-port console logs.
	LogDir string `json:"log_dir,omitempty"`
	// AuditDir is the root for the JSONL audit trail.
	AuditDir string `json:"audit_dir,omitempty"`

	// Baud is the default line rate.
	Baud int `json:"baud,omitempty"`
	// StampLogs prefixes each log line with a timestamp. Disable for logs you
	// intend to replay through a terminal emulator verbatim.
	StampLogs *bool `json:"stamp_logs,omitempty"`
	// AllowObserve permits read-only attachments. When false a port is
	// exclusive to its owner.
	AllowObserve *bool `json:"allow_observe,omitempty"`
	// MaxObservers caps concurrent read-only attachments, 0 for unlimited.
	MaxObservers int `json:"max_observers,omitempty"`
	// AutoOpen opens every discovered port at startup so logging runs even with
	// nobody attached. This is what makes the daemon worth having.
	AutoOpen *bool `json:"auto_open,omitempty"`
	// ScanGlobs are extra device globs scanned on Linux, on top of
	// /dev/serial/by-id. Ignored on Windows, which enumerates the registry.
	ScanGlobs []string `json:"scan_globs,omitempty"`

	// Ports pins per-port settings.
	Ports []PortDef `json:"ports,omitempty"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	yes := true
	logDir, auditDir := defaultDirs()
	return Config{
		LogDir:       logDir,
		AuditDir:     auditDir,
		Baud:         115200,
		StampLogs:    &yes,
		AllowObserve: &yes,
		AutoOpen:     &yes,
		MaxObservers: 4,
	}
}

// Path returns the configuration file location.
func Path() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: locate user config directory: %w", err)
	}
	return filepath.Join(d, "sercon", "config.json"), nil
}

// Load reads the configuration, applying defaults for anything absent.
//
// Neither a missing file nor an unlocatable config directory is an error.
// Every setting already has a working default, and refusing to run because
// %AppData% happens to be unset would be a poor trade for a console tool.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		p, err := Path()
		if err != nil {
			return cfg, nil
		}
		path = p
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

// normalize re-applies defaults for any zero value a partial file left behind,
// then expands relative paths so a daemon started from an arbitrary working
// directory still writes where the operator expects.
func (c *Config) normalize() {
	def := Defaults()
	if c.Baud == 0 {
		c.Baud = def.Baud
	}
	if c.StampLogs == nil {
		c.StampLogs = def.StampLogs
	}
	if c.AllowObserve == nil {
		c.AllowObserve = def.AllowObserve
	}
	if c.AutoOpen == nil {
		c.AutoOpen = def.AutoOpen
	}
	if c.LogDir == "" {
		c.LogDir = def.LogDir
	}
	if c.AuditDir == "" {
		c.AuditDir = filepath.Join(filepath.Dir(c.LogDir), "audit")
	}
	c.LogDir = expand(c.LogDir)
	c.AuditDir = expand(c.AuditDir)
}

// Aliases indexes the pinned port definitions by reference.
func (c Config) Aliases() map[string]PortDef {
	out := make(map[string]PortDef, len(c.Ports))
	for _, p := range c.Ports {
		if p.Ref != "" {
			out[p.Ref] = p
		}
	}
	return out
}

// LogsStamped reports whether log lines carry timestamps.
func (c Config) LogsStamped() bool { return c.StampLogs == nil || *c.StampLogs }

// ObserversAllowed reports whether read-only attachment is permitted.
func (c Config) ObserversAllowed() bool { return c.AllowObserve == nil || *c.AllowObserve }

// OpensAutomatically reports whether ports are opened at daemon start.
func (c Config) OpensAutomatically() bool { return c.AutoOpen == nil || *c.AutoOpen }

// defaultDirs picks per-platform state locations. This is a runtime switch
// rather than build tags because every call used here exists on every platform.
func defaultDirs() (logDir, auditDir string) {
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		base = filepath.Join(base, "sercon")
		return filepath.Join(base, "ports"), filepath.Join(base, "audit")
	default:
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "state")
		}
		base = filepath.Join(base, "sercon")
		return filepath.Join(base, "ports"), filepath.Join(base, "audit")
	}
}

// expand resolves a leading ~ and makes the path absolute.
func expand(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || len(p) > 1 && (p[0] == '~' && (p[1] == '/' || p[1] == '\\')) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
