package config

import (
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
)

const appName = "rabbot"

// ConfigDirPath resolves the base config directory the same way ResolveConfigDir
// does — an explicitly-set XDG_CONFIG_HOME override (honored on EVERY OS, per the
// XDG Base Directory spec), else the per-OS default — but WITHOUT creating it.
//
// The override is honored on all OSes (Linux/macOS/Windows), not just Linux:
// os.UserConfigDir consults $XDG_CONFIG_HOME only on Unix, so on macOS/Windows it
// would silently ignore the override and resolve the real user dir. Checking the
// env var first restores the documented "env-vars-first" contract everywhere,
// which keeps tests hermetic on every runner. When the override is unset we defer
// to os.UserConfigDir for the exact, unchanged per-OS default (Linux
// $HOME/.config; macOS ~/Library/Application Support; Windows %AppData%).
func ConfigDirPath() (string, error) {
	if override := os.Getenv("XDG_CONFIG_HOME"); override != "" && filepath.IsAbs(override) {
		return filepath.Join(override, appName), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}

// ResolveConfigDir returns the per-OS config directory, creating it if needed.
// An explicitly-set $XDG_CONFIG_HOME override wins on every OS; otherwise the
// per-OS default is used — Linux: $HOME/.config/rabbot; macOS:
// ~/Library/Application Support/rabbot; Windows: %AppData%\rabbot.
func ResolveConfigDir() (string, error) {
	dir, err := ConfigDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

// DataDirPath resolves the data directory the same way ResolveDataDir does —
// the override when set, else the per-OS default — but WITHOUT creating it.
// Read-only CLI queries use this so they open the same DB the daemon writes to.
func DataDirPath(override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(xdg.DataHome, appName)
}

// ResolveDataDir returns the data directory. If override is non-empty it is
// returned verbatim (after ensuring it exists). Otherwise the per-OS data dir
// (Linux $XDG_DATA_HOME/rabbot; Windows %LocalAppData%\rabbot; macOS
// Application Support) is used.
func ResolveDataDir(override string) (string, error) {
	dir := DataDirPath(override)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

// ConfigFilePath returns the path to config.yaml within the given config dir.
func ConfigFilePath(configDir string) string {
	return filepath.Join(configDir, "config.yaml")
}
