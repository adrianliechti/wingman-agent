//go:build darwin

package shell

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const sandboxExecPath = "/usr/bin/sandbox-exec"

func platformSandboxCommand(shell, command, _ string, policy sandboxPolicy) (string, []string, error) {
	if info, err := os.Stat(sandboxExecPath); err != nil || info.IsDir() {
		return "", nil, fmt.Errorf("workspace shell sandbox requires %s", sandboxExecPath)
	}
	profile, definitions := seatbeltProfile(policy)
	args := []string{"-p", profile}
	args = append(args, definitions...)
	args = append(args, "--", shell)
	// zsh lowers the priority of background jobs, which the sandbox refuses;
	// without this every `&` prints "nice(5) failed: operation not permitted".
	if filepath.Base(shell) == "zsh" {
		args = append(args, "-o", "nobgnice")
	}
	args = append(args, shellArguments(shell, command)...)
	return sandboxExecPath, args, nil
}

// seatbeltProfile starts closed by default, permits the minimum runtime
// services needed by ordinary CLI programs, then opens writes only beneath
// parameterized roots. In particular, it does not use the broad mach*, ipc*,
// iokit-open, or file-ioctl grants that make many ad-hoc Seatbelt profiles
// ineffective. The service allowlist follows the same shape as the hardened
// policy used by Anthropic Sandbox Runtime, narrowed to Wingman's
// workspace-write mode. Apple Events and Launch Services operations stay
// blocked because either can ask an unsandboxed GUI process to act on behalf
// of the sandboxed command. Host Unix sockets stay unreachable, including
// sockets inside writable roots; IP networking only adds the DNS service.
// F_MAKECOMPRESSED (80) and F_TRANSFEREXTENTS (110) change a file through a
// read-only descriptor, which neither file-write* nor file-ioctl covers, so
// they need their own deny even under deny default.
func seatbeltProfile(policy sandboxPolicy) (string, []string) {
	var definitions []string
	paramRules := func(prefix string, paths []string) string {
		var rules strings.Builder
		for i, path := range paths {
			name := fmt.Sprintf("%s_%d", prefix, i)
			fmt.Fprintf(&rules, " (literal (param %q)) (subpath (param %q))", name, name)
			definitions = append(definitions, "-D"+name+"="+path)
		}
		return rules.String()
	}
	writeRules := paramRules("WRITABLE_ROOT", policy.WritableRoots) + paramRules("SHARED_TEMP", policy.SharedTemp)

	var profile strings.Builder
	profile.WriteString(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow process-info* (target same-sandbox))
(allow signal (target same-sandbox))
(allow mach-priv-task-port (target same-sandbox))
(allow user-preference-read)
(allow mach-lookup
 (global-name "com.apple.audio.systemsoundserver")
 (global-name "com.apple.distributed_notifications@Uv3")
 (global-name "com.apple.FontObjectsServer")
 (global-name "com.apple.fonts")
 (global-name "com.apple.logd")
 (global-name "com.apple.lsd.mapdb")
 (global-name "com.apple.PowerManagement.control")
 (global-name "com.apple.system.logger")
 (global-name "com.apple.system.notification_center")
 (global-name "com.apple.system.opendirectoryd.libinfo")
 (global-name "com.apple.system.opendirectoryd.membership")
 (global-name "com.apple.bsd.dirhelper")
 (global-name "com.apple.securityd.xpc")
 (global-name "com.apple.SecurityServer")
 (global-name "com.apple.coreservices.launchservicesd"))
(allow ipc-posix-shm)
(allow ipc-posix-sem)
(allow iokit-open
 (iokit-registry-entry-class "IOSurfaceRootUserClient")
 (iokit-registry-entry-class "RootDomainUserClient")
 (iokit-user-client-class "IOSurfaceSendRight"))
(allow iokit-get-properties)
(allow system-socket
 (require-all (socket-domain AF_SYSTEM) (socket-protocol 2)))
(allow sysctl-read)
(allow sysctl-write (sysctl-name "kern.tcsm_enable"))
(allow distributed-notification-post)
(allow file-read*)
(allow file-write*` + writeRules + `
 (literal "/dev/null")
 (literal "/dev/zero")
 (literal "/dev/random")
 (literal "/dev/urandom")
 (literal "/dev/tty")
 (subpath "/dev/fd")
 (literal "/dev/ptmx")
 (regex #"^/dev/ttys[0-9]+"))
(allow file-ioctl
 (literal "/dev/null")
 (literal "/dev/zero")
 (literal "/dev/random")
 (literal "/dev/urandom")
 (literal "/dev/dtracehelper")
 (literal "/dev/tty")
 (literal "/dev/ptmx")
 (regex #"^/dev/ttys"))
(allow pseudo-tty)
(deny appleevent-send)
(deny lsopen)
(deny system-fcntl (fcntl-command 80 110))
`)
	if policy.Network {
		profile.WriteString(seatbeltNetwork)
	} else if policy.Loopback {
		profile.WriteString(seatbeltLoopback)
	}
	var protectedPaths []string
	for _, root := range sandboxProtectedRoots(policy) {
		profile.WriteString(seatbeltProtectedRules(root))
		protectedPaths = append(protectedPaths, existingProtectedPaths(root)...)
		for _, dir := range protectedAncestors(root) {
			profile.WriteString("(deny file-write-unlink (literal " + seatbeltString(dir) + "))\n")
		}
	}
	if rules := paramRules("PROTECTED_PATH", protectedPaths); rules != "" {
		profile.WriteString("(deny file-write*" + rules + ")\n")
	}
	// Temp skips the protected-root rules, but swapping its root for a link would redirect the next command's grant.
	if policy.TempDir != "" {
		profile.WriteString("(deny file-write-unlink (literal " + seatbeltString(policy.TempDir) + "))\n")
	}
	if denyRules := paramRules("DENY_READ", policy.DenyRead); denyRules != "" {
		profile.WriteString("(deny file-read*" + denyRules + ")\n(deny file-write*" + denyRules + ")\n")
	}
	return profile.String(), definitions
}

// seatbeltLoopback lets commands run their own servers and tests on loopback
// while outside hosts stay unreachable. It also reaches every other service
// listening on this machine, the agent's own web UI included: Seatbelt cannot
// exclude single ports reliably.
const seatbeltLoopback = `(allow network-bind (local ip "localhost:*"))
(allow network-inbound (local ip "localhost:*"))
(allow network-outbound (remote ip "localhost:*"))
`

// seatbeltNetwork allows IP traffic plus the DNS and TLS trust services it needs, following Codex.
const seatbeltNetwork = `(allow network-outbound (remote ip "*:*"))
(allow network-inbound (local ip "*:*"))
(allow network-bind (local ip "*:*"))
(allow network-outbound (literal "/private/var/run/mDNSResponder"))
(allow mach-lookup
 (global-name "com.apple.networkd")
 (global-name "com.apple.ocspd")
 (global-name "com.apple.trustd.agent")
 (global-name "com.apple.SystemConfiguration.DNSConfiguration")
 (global-name "com.apple.SystemConfiguration.configd"))
(allow sysctl-read (sysctl-name-regex #"^net.routetable"))
`

// seatbeltProtectedRules denies writes to protected paths at any depth and
// blocks renaming the workspace, which would sidestep the path rules.
func seatbeltProtectedRules(root string) string {
	prefix := "^" + regexp.QuoteMeta(root) + "/"
	var filters []string
	for _, dir := range protectedDirs {
		filters = append(filters, seatbeltRegex(prefix+"(.*/)?"+regexp.QuoteMeta(dir)+"(/.*)?$"))
	}
	for _, file := range protectedFiles {
		filters = append(filters, seatbeltRegex(prefix+"(.*/)?"+regexp.QuoteMeta(file)+"$"))
	}
	for _, file := range protectedRootFiles {
		filters = append(filters, seatbeltRegex(prefix+regexp.QuoteMeta(file)+"$"))
	}
	return "(deny file-write*\n " + strings.Join(filters, "\n ") + ")\n" +
		"(deny file-write-unlink (literal " + seatbeltString(root) + "))\n"
}

const (
	// The scan runs before every command, so it reads a bounded number of
	// directory entries, nearest the root first.
	protectedScanEntries = 20000
	// The profile travels as a command-line argument.
	protectedAncestorLimit = 512
)

// Dependency and version-control trees are large and hold no configuration
// of the user's.
var protectedScanSkips = []string{".git", ".hg", ".svn", ".jj", "node_modules"}

// protectedAncestors lists the directories between root and the protected
// paths that exist beneath it. The name rules follow a path, not a file:
// renaming such a directory into another writable root, such as private temp,
// would carry its protected descendants out from under them, to be edited and
// moved back. The scan does not follow links and is bounded; a protected path
// it does not reach keeps its write protection but not this one. Only existing
// configuration is pinned: a directory staged in private temp can still be
// moved in with new protected names inside it.
func protectedAncestors(root string) []string {
	root = filepath.Clean(root)
	var ancestors []string
	queue := []string{root}
	for budget := protectedScanEntries; len(queue) > 0 && budget > 0; {
		dir := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		budget -= len(entries)
		pinned := false
		for _, entry := range entries {
			name := entry.Name()
			switch {
			case slices.Contains(protectedDirs, name) || slices.Contains(protectedFiles, name):
				pinned = true
			case entry.IsDir() && !slices.Contains(protectedScanSkips, name):
				queue = append(queue, filepath.Join(dir, name))
			}
		}
		for ; pinned && len(dir) > len(root) && !slices.Contains(ancestors, dir); dir = filepath.Dir(dir) {
			if len(ancestors) == protectedAncestorLimit {
				return ancestors
			}
			ancestors = append(ancestors, dir)
		}
	}
	return ancestors
}

func seatbeltRegex(pattern string) string {
	return "(regex " + seatbeltString(pattern) + ")"
}

func seatbeltString(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
