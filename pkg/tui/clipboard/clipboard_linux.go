//go:build linux

package clipboard

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

var linuxImageTypes = []string{
	"image/png",
	"image/jpeg",
	"image/webp",
	"image/gif",
}

func readText() (string, error) {
	if uris, err := exec.Command("wl-paste", "--no-newline", "-t", "text/uri-list").Output(); err == nil && strings.TrimSpace(string(uris)) != "" {
		return string(uris), nil
	}
	output, waylandErr := exec.Command("wl-paste", "--no-newline").Output()
	if waylandErr == nil {
		return string(output), nil
	}

	if uris, err := exec.Command("xclip", "-selection", "clipboard", "-t", "text/uri-list", "-o").Output(); err == nil && strings.TrimSpace(string(uris)) != "" {
		return string(uris), nil
	}
	output, x11Err := exec.Command("xclip", "-selection", "clipboard", "-o").Output()
	if x11Err == nil {
		return string(output), nil
	}

	output, xselErr := exec.Command("xsel", "--clipboard", "--output").Output()
	if xselErr == nil {
		return string(output), nil
	}

	return "", fmt.Errorf("wl-paste: %v; xclip: %v; xsel: %w", waylandErr, x11Err, xselErr)
}

func readImage() (string, error) {
	var waylandErr, x11Err error

	for _, mime := range linuxImageTypes {
		data, err := exec.Command("wl-paste", "-t", mime).Output()
		if err == nil && len(data) > 0 {
			return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
		}
		if waylandErr == nil && err != nil {
			waylandErr = err
		}
	}

	for _, mime := range linuxImageTypes {
		data, err := exec.Command("xclip", "-selection", "clipboard", "-t", mime, "-o").Output()
		if err == nil && len(data) > 0 {
			return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
		}
		if x11Err == nil && err != nil {
			x11Err = err
		}
	}

	if waylandErr != nil && x11Err != nil {
		return "", fmt.Errorf("wl-paste: %v; xclip: %w", waylandErr, x11Err)
	}

	return "", nil
}

func writeNativeText(text string) error {

	cmd := exec.Command("wl-copy")
	cmd.Stdin = strings.NewReader(text)

	if err := cmd.Run(); err == nil {
		return nil
	}

	cmd = exec.Command("xclip", "-selection", "clipboard")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err == nil {
		return nil
	}

	cmd = exec.Command("xsel", "--clipboard", "--input")
	cmd.Stdin = strings.NewReader(text)

	return cmd.Run()
}

