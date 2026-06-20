package wizard

// CadenceChoice is a friendly "how often should we check?" option that maps to
// concrete min/max recheck intervals, so a non-technical user never types a Go
// duration. "Custom" is handled by the caller (a raw duration input for experts).
type CadenceChoice int

const (
	CadenceFewTimesADay CadenceChoice = iota // default
	CadenceAboutHourly
	CadenceFewTimesAnHour
)

// cadenceLabel is the friendly menu text for each choice. It is the SINGLE source
// of truth shared by the fine-tune Select (CadenceLabels) and the reverse mapping
// (parseCadence), so the screen and the parser can never drift out of sync.
var cadenceLabel = map[CadenceChoice]string{
	CadenceFewTimesADay:   "A few times a day (default)",
	CadenceAboutHourly:    "About hourly",
	CadenceFewTimesAnHour: "A few times an hour",
}

// cadenceOrder fixes the menu order (default first), so CadenceLabels is
// deterministic rather than ranging a map.
var cadenceOrder = []CadenceChoice{CadenceFewTimesADay, CadenceAboutHourly, CadenceFewTimesAnHour}

// CadenceLabels returns the friendly cadence options in menu order. The fine-tune
// Select builds its huh options from this list and feeds the chosen label back to
// parseCadence, so the two stay in lockstep.
func CadenceLabels() []string {
	out := make([]string, 0, len(cadenceOrder))
	for _, c := range cadenceOrder {
		out = append(out, cadenceLabel[c])
	}
	return out
}

// CadenceIntervals maps a choice to (minInterval, maxInterval) Go-duration
// strings the config writers accept. The floor (min) stays conservative; the
// choice tunes the ceiling (max) — how long a site may go between rechecks.
func CadenceIntervals(c CadenceChoice) (min, max string) {
	switch c {
	case CadenceAboutHourly:
		return "10m", "1h"
	case CadenceFewTimesAnHour:
		return "10m", "20m"
	default: // CadenceFewTimesADay
		return "10m", "24h"
	}
}

// ParseCadence maps a friendly menu label back to its choice for the cli runner;
// ok=false for an unknown label (the caller then leaves cadence untouched). It is
// the exported entry point over the unit-tested parseCadence core, so the
// label→choice mapping lives in one place rather than being duplicated in cli.
func ParseCadence(label string) (CadenceChoice, string, bool) {
	return parseCadence(label)
}

// parseCadence maps a menu label back to its choice; ok=false for an unknown
// label (the caller then leaves cadence untouched). The returned label is echoed
// verbatim so the caller can render it without re-deriving.
func parseCadence(label string) (CadenceChoice, string, bool) {
	for c, l := range cadenceLabel {
		if l == label {
			return c, label, true
		}
	}
	return 0, label, false
}
