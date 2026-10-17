package clipboard

import (
	"encoding/base64"
	"os/exec"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/process"
)

// Shared by native Windows and the WSL paste fallback.
const powerShellEncodingPrefix = "[Console]::InputEncoding = [System.Text.Encoding]::UTF8; [Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "

func buildPowerShellArgs(script string, sta bool) []string {
	args := []string{"-NoProfile", "-NoLogo", "-NonInteractive"}
	if sta {
		args = append(args, "-Sta")
	}
	return append(args, "-Command", powerShellEncodingPrefix+script)
}

func runPowerShell(script string, sta bool) ([]byte, error) {
	cmd := exec.Command("powershell.exe", buildPowerShellArgs(script, sta)...)
	process.Hide(cmd)
	return cmd.Output()
}

func readWindowsClipboard() ([]Content, error) {
	return readWithFallback(readPowerShellText, readPowerShellImage, nil)
}

func readPowerShellText() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms; $files = [System.Windows.Forms.Clipboard]::GetFileDropList(); if ($files -and $files.Count -gt 0) { [Console]::Out.Write(($files -join "` + "`n" + `")) } else { $text = Get-Clipboard -Format Text -Raw -ErrorAction SilentlyContinue; if ($null -ne $text) { [Console]::Out.Write($text) } }`
	output, err := runPowerShell(script, true)
	if err != nil {
		return "", err
	}
	text := string(output)
	if text != "" && isWSL() {
		text = convertWSLClipboardPaths(text)
	}
	return text, nil
}

func convertWSLClipboardPaths(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var converted []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !looksLikeWindowsDrivePath(trimmed) {
			return text
		}
		out, err := exec.Command("wslpath", "-u", trimmed).Output()
		if err != nil {
			return text
		}
		converted = append(converted, strings.TrimSpace(string(out)))
	}
	if len(converted) == 0 {
		return text
	}
	return strings.Join(converted, "\n")
}

func looksLikeWindowsDrivePath(s string) bool {
	if len(s) < 3 {
		return false
	}
	d := s[0]
	if (d < 'A' || d > 'Z') && (d < 'a' || d > 'z') {
		return false
	}
	return s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

func readPowerShellImage() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms; $img = [System.Windows.Forms.Clipboard]::GetImage(); if ($img) { $ms = New-Object System.IO.MemoryStream; $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png); [System.Convert]::ToBase64String($ms.ToArray()) }`
	output, err := runPowerShell(script, true)
	if err != nil {
		return "", err
	}
	data := strings.TrimSpace(string(output))
	if data == "" {
		return "", nil
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return "", err
	}
	return "data:image/png;base64," + data, nil
}
