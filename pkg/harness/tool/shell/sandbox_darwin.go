//go:build darwin

package shell

import (
	"fmt"
	"os"
	"regexp"
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
	args = append(args, "--", shell, "-c", command)
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
	writeRules := paramRules("WRITABLE_ROOT", policy.WritableRoots)

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
`)
	if policy.Network {
		profile.WriteString(seatbeltNetwork)
	}
	var protectedPaths []string
	for _, root := range sandboxProtectedRoots(policy) {
		profile.WriteString(seatbeltProtectedRules(root))
		protectedPaths = append(protectedPaths, existingProtectedPaths(root)...)
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

func seatbeltRegex(pattern string) string {
	return "(regex " + seatbeltString(pattern) + ")"
}

func seatbeltString(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
