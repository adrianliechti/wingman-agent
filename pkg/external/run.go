package external

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
)

type Options struct {
	Path string
	Env  []string

	WingmanURL   string
	WingmanToken string
}

// MergeEnv replaces launcher-owned variables and removes obsolete overrides.
// A nil parent inherits the current environment; an empty parent stays empty.
func MergeEnv(parent []string, overrides map[string]string, remove ...string) []string {
	if parent == nil {
		parent = os.Environ()
	}
	env := make([]string, 0, len(parent)+len(overrides))
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !overridden && !slices.Contains(remove, key) {
			env = append(env, entry)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		env = append(env, key+"="+overrides[key])
	}
	return env
}

// Run starts a CLI with the caller's terminal streams.
func Run(ctx context.Context, path string, args, env []string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
