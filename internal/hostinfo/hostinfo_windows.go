//go:build windows

package hostinfo

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemPowerStatus mirrors the Win32 SYSTEM_POWER_STATUS struct. Only BatteryFlag
// is read; the remaining fields are present so the layout the kernel writes into is
// correct. The two trailing DWORDs (BatteryLifeTime/BatteryFullLifeTime) are uint32.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// batteryFlagNoBattery is the SYSTEM_POWER_STATUS.BatteryFlag bit (128) the kernel
// sets when there is no system battery. Its absence ⇒ a battery is present.
const batteryFlagNoBattery = 128

var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	getSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")
)

// sleeper calls kernel32!GetSystemPowerStatus and reports a battery present when the
// "no battery" flag (128) is clear. Any failure — DLL/proc load, a zero return, or an
// "unknown" (255) BatteryFlag — yields false (criterion 7: unknown ⇒ silent).
func sleeper() bool {
	if err := getSystemPowerStatus.Find(); err != nil {
		return false
	}
	var sps systemPowerStatus
	// G103: passing a pointer to a local struct into a Win32 syscall is the only way
	// to receive the SYSTEM_POWER_STATUS output; the struct outlives the call and the
	// kernel writes only its fixed-size fields. Audited and safe.
	r1, _, _ := getSystemPowerStatus.Call(uintptr(unsafe.Pointer(&sps))) //nolint:gosec // audited Win32 syscall pointer pass
	if r1 == 0 {
		// Call failed; treat as unknown.
		return false
	}
	if sps.BatteryFlag == 255 {
		// 255 ("unknown") ⇒ status cannot be determined; stay silent.
		return false
	}
	return sps.BatteryFlag&batteryFlagNoBattery == 0
}
