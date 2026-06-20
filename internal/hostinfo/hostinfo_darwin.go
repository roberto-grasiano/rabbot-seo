//go:build darwin

package hostinfo

import (
	"strings"

	"golang.org/x/sys/unix"
)

// sleeper returns true when `hw.model` names a portable Mac (a "MacBook*").
// Desktops (Mac mini, iMac, Mac Studio, Mac Pro) report non-MacBook models and so
// are treated as always-on. A Sysctl error yields false (criterion 7).
func sleeper() bool {
	model, err := unix.Sysctl("hw.model")
	if err != nil {
		return false
	}
	return strings.HasPrefix(model, "MacBook")
}
