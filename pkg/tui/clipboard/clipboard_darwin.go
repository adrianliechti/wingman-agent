//go:build darwin

package clipboard

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func readText() (string, error) {
	if paths, err := readFinderFilePaths(); err == nil && len(paths) > 0 {
		return strings.Join(paths, "\n"), nil
	}

	output, err := exec.Command("pbpaste").Output()

	if err != nil {
		return "", err
	}

	return string(output), nil
}

func readFinderFilePaths() ([]string, error) {
	const script = `
ObjC.import("AppKit");
const pb = $.NSPasteboard.generalPasteboard;
const classes = $.NSArray.arrayWithObject($.NSURL.class);
const urls = pb.readObjectsForClassesOptions(classes, $());
const paths = [];
if (urls) {
	const n = urls.count;
	for (let i = 0; i < n; i++) {
		const u = urls.objectAtIndex(i);
		if (u && u.isFileURL) {
			const p = ObjC.unwrap(u.path);
			if (p) paths.push(p);
		}
	}
}
JSON.stringify(paths);
`
	output, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &paths); err != nil {
		return nil, err
	}
	return paths, nil
}

func readImage() (string, error) {
	// If the clipboard holds copied file URLs (e.g. from Finder), let readText()
	// provide the full file paths instead of extracting the Finder file icon.
	if paths, err := readFinderFilePaths(); err == nil && len(paths) > 0 {
		return "", nil
	}

	tmpFile, err := os.CreateTemp("", "clipboard-*.png")

	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	script := fmt.Sprintf(`
		set tmpFile to POSIX file "%s"
		try
			set imageData to the clipboard as «class PNGf»
			set fileRef to open for access tmpFile with write permission
			write imageData to fileRef
			close access fileRef

			return "ok"
		on error

			return "no image"
		end try
	`, tmpPath)

	output, err := exec.Command("osascript", "-e", script).Output()

	if err != nil {
		return "", err
	}
	if string(output) != "ok\n" {
		return "", nil
	}

	data, err := os.ReadFile(tmpPath)

	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}

	encoded := base64.StdEncoding.EncodeToString(data)

	return "data:image/png;base64," + encoded, nil
}

func writeNativeText(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)

	return cmd.Run()
}
