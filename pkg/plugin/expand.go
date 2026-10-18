package plugin

import (
	"runtime"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/external"
)

const (
	rootVariable = "PLUGIN_ROOT"
	dataVariable = "PLUGIN_DATA"

	rootPlaceholder = "${" + rootVariable + "}"
	dataPlaceholder = "${" + dataVariable + "}"
)

func expand(value, root, data string) string {
	return external.Expand(value, map[string]string{rootVariable: root, dataVariable: data})
}

// sameVariable compares environment variable names using the platform's own
// semantics; Windows treats them case-insensitively.
func sameVariable(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}

	return a == b
}
