//go:build darwin

package main

import (
	"os/exec"
	"strings"
)

func isDarkMode() bool {
	out, err := exec.Command("defaults", "read", "-g", "AppleInterfaceStyle").Output()
	return err == nil && strings.TrimSpace(string(out)) == "Dark"
}
