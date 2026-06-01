package external

import (
	"os"
	"strings"
)

func LookupPath(tool, fallback string) string {
	key := "WINGMAN_" + strings.ToUpper(strings.ReplaceAll(tool, "-", "_")) + "_PATH"

	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return fallback
}
