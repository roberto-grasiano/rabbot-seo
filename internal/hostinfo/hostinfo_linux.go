//go:build linux

package hostinfo

import (
	"os"
	"path/filepath"
	"strings"
)

// sysfsPowerSupplyRoot is the sysfs directory Linux exposes power supplies under.
// It is a package-level var so tests inject a fake tree; production reads the real
// kernel path. Each child dir carries a `type` file whose contents are "Battery",
// "Mains", "USB", etc.
var sysfsPowerSupplyRoot = "/sys/class/power_supply"

// sleeper returns true when any power supply under sysfsPowerSupplyRoot reports
// type "Battery". Every failure mode — unreadable root, missing files, read errors —
// yields false, never an error or panic (criterion 7).
func sleeper() bool {
	entries, err := os.ReadDir(sysfsPowerSupplyRoot)
	if err != nil {
		return false
	}
	for _, e := range entries {
		// os.ReadFile follows symlinks, which is what sysfs entries are.
		b, err := os.ReadFile(filepath.Join(sysfsPowerSupplyRoot, e.Name(), "type"))
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(b)), "Battery") {
			return true
		}
	}
	return false
}
