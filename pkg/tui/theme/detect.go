package theme

import (
	"os"
	"strconv"
	"strings"
)

func isLightBackground() bool {
	return detectLightBackground(os.Getenv("WINGMAN_THEME"), os.Getenv("COLORFGBG"), queryTerminalBackground)
}

func detectLightBackground(setting, colorFGBG string, query func() (light, known bool)) bool {
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "light":
		return true
	case "dark":
		return false
	}

	// A terminal reply reflects the current profile. COLORFGBG can be stale
	// when a window's appearance changes after the shell was started.
	if light, known := query(); known {
		return light
	}
	parts := strings.Split(colorFGBG, ";")
	if len(parts) >= 2 {
		bg, err := strconv.Atoi(parts[len(parts)-1])
		return err == nil && (bg == 7 || bg == 15)
	}
	return false
}
