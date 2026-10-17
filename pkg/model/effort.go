package model

import "slices"

var effortLevels = []string{"none", "low", "medium", "high", "xhigh", "max"}

func EffortLevels() []string {
	return slices.Clone(effortLevels)
}

// ClampEffort chooses the closest supported level at or below the request,
// or the lowest supported level when the request is below it. Supported levels
// must be in ascending order. Empty or unknown requests pass through unchanged.
func ClampEffort(value string, supported []string) string {
	if len(supported) == 0 || slices.Contains(supported, value) {
		return value
	}
	rank := slices.Index(effortLevels, value)
	if rank < 0 {
		return value
	}
	clamped := supported[0]
	for _, level := range supported {
		levelRank := slices.Index(effortLevels, level)
		if levelRank < 0 {
			continue
		}
		if levelRank > rank {
			break
		}
		clamped = level
	}
	return clamped
}
