package codex

import (
	"slices"

	"github.com/coder/acp-go-sdk"
)

// JetBrains AIR extensions travel in _meta.jetbrains.air and are negotiated by
// listing capability names during initialize.
const (
	airVersion                 = 1
	recommendedValueCapability = "recommendedValue"
)

func airMeta(key string, value any) map[string]any {
	return map[string]any{"jetbrains": map[string]any{"air": map[string]any{"version": airVersion, key: value}}}
}

func airCapabilitiesMeta() map[string]any {
	return airMeta("capabilities", []string{recommendedValueCapability})
}

func clientSupportsAirCapability(caps acp.ClientCapabilities, capability string) bool {
	jetbrains, _ := caps.Meta["jetbrains"].(map[string]any)
	air, _ := jetbrains["air"].(map[string]any)
	var version float64
	switch v := air["version"].(type) {
	case float64:
		version = v
	case int:
		version = float64(v)
	default:
		return false
	}
	if version < airVersion || version != float64(int64(version)) {
		return false
	}
	switch supported := air["capabilities"].(type) {
	case []string:
		return slices.Contains(supported, capability)
	case []any:
		return slices.Contains(supported, any(capability))
	}
	return false
}
