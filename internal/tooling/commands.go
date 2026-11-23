// Package tooling finds the commands of language servers, also in the
// standard tool directories a GUI process does not have on its PATH.
package tooling

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func Candidates(goos, command string) []string {
	if goos == "windows" {
		return []string{command + ".exe", command + ".cmd", command + ".bat", command}
	}
	return []string{command}
}

func Executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode()&0o111 != 0
}

func Find(directories []string, command string) string {
	for _, directory := range directories {
		for _, name := range Candidates(runtime.GOOS, command) {
			candidate := filepath.Join(directory, name)
			if Runnable(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// Runnable also validates the interpreter of a script launcher so a broken
// script is not treated as an available command.
func Runnable(path string) bool {
	if !Executable(path) || runtime.GOOS == "windows" {
		return Executable(path)
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	line, _ := bufio.NewReader(file).ReadString('\n')
	if !strings.HasPrefix(line, "#!") {
		return true
	}
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "#!"))
	if len(fields) == 0 || !filepath.IsAbs(fields[0]) || !Executable(fields[0]) {
		return false
	}
	if filepath.Base(fields[0]) != "env" || len(fields) == 1 {
		return true
	}
	interpreter := ""
	for _, field := range fields[1:] {
		if strings.HasPrefix(field, "-") || strings.Contains(field, "=") {
			continue
		}
		interpreter = field
		break
	}
	if interpreter == "" {
		return false
	}
	if path, err := exec.LookPath(interpreter); err == nil && Executable(path) {
		return true
	}
	return findExecutable(userDirectories(), interpreter) != ""
}

func findExecutable(directories []string, command string) string {
	for _, directory := range directories {
		for _, name := range Candidates(runtime.GOOS, command) {
			candidate := filepath.Join(directory, name)
			if Executable(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func LookPath(command string) (string, error) {
	if filepath.IsAbs(command) {
		if Runnable(command) {
			return command, nil
		}
		return "", exec.ErrNotFound
	}
	if path, err := exec.LookPath(command); err == nil && Runnable(path) {
		return path, nil
	}
	if path := Find(userDirectories(), command); path != "" {
		return path, nil
	}
	return "", exec.ErrNotFound
}

func Resolve(command string) string {
	path, _ := LookPath(command)
	return path
}

func userDirectories() []string {
	var directories []string
	add := func(path string) {
		if path == "" {
			return
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			directories = append(directories, path)
		}
	}
	add(os.Getenv("GOBIN"))
	if value := os.Getenv("GOPATH"); value != "" {
		for _, root := range filepath.SplitList(value) {
			add(filepath.Join(root, "bin"))
		}
	}
	add(os.Getenv("PNPM_HOME"))

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return directories
	}
	for _, path := range []string{
		filepath.Join(home, "go", "bin"), filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, ".local", "bin"), filepath.Join(home, ".dotnet", "tools"),
		filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".deno", "bin"),
		filepath.Join(home, ".volta", "bin"), filepath.Join(home, ".asdf", "shims"),
		filepath.Join(home, ".local", "share", "mise", "shims"), filepath.Join(home, ".npm-global", "bin"),
	} {
		add(path)
	}
	if runtime.GOOS == "windows" {
		add(filepath.Join(home, "scoop", "shims"))
		if value := os.Getenv("APPDATA"); value != "" {
			add(filepath.Join(value, "npm"))
		}
		if value := os.Getenv("LOCALAPPDATA"); value != "" {
			for _, path := range []string{
				filepath.Join(value, "nvim-data", "mason", "bin"), filepath.Join(value, "pnpm"),
				filepath.Join(value, "Volta", "bin"), filepath.Join(value, "Microsoft", "WinGet", "Links"),
			} {
				add(path)
			}
		}
		if value := os.Getenv("PROGRAMDATA"); value != "" {
			add(filepath.Join(value, "chocolatey", "bin"))
		}
		return directories
	}
	for _, path := range []string{
		filepath.Join(home, ".local", "share", "nvim", "mason", "bin"), filepath.Join(home, "Library", "pnpm"),
		filepath.Join(home, ".local", "share", "pnpm"), "/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin",
	} {
		add(path)
	}
	return directories
}
