//go:build !windows

package shell

func platformSandboxCheck(workspace string, policy sandboxPolicy) error {
	_, _, err := platformSandboxCommand("/bin/sh", "true", workspace, policy)
	return err
}
