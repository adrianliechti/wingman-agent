//go:build windows

package shell

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows sandbox runs the shell on a Low integrity, write-restricted
// token. Reads are checked as for the user; writes must also pass a second
// check against only the restricting SIDs, the main one being a SID unique to
// the workspace. Writable roots therefore carry a Low label plus an ACE for
// that SID, protected paths a deny ACE for it, and credentials a Medium
// NO_READ_UP label. The ACLs are applied in place and left there, so the
// cost of propagating them through a large tree is paid once.
//
// The process is created by a helper, this executable re-exec'd with
// sandboxHelperArg, which keeps the shell and its descendants in a
// kill-on-close job: killing the helper's process tree kills the sandbox.
//
// AppContainers (as MXC uses) would also cut the network, but they cannot
// stat drive roots without an elevated host change (PowerShell then loads no
// modules), and AppContainer ACEs on a directory make it unreadable for
// restricted tokens, so the two cannot share a workspace.
const sandboxHelperArg = "__wingman-sandbox-exec"

func init() {
	if len(os.Args) == 3 && os.Args[1] == sandboxHelperArg {
		os.Exit(runSandboxHelper(os.Args[2]))
	}
}

type sandboxLaunch struct {
	SID  string   `json:"sid"`
	Temp string   `json:"temp"`
	Argv []string `json:"argv"`
}

const (
	sandboxRWMask   = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE | windows.DELETE
	fileDeleteChild = 0x40
	// Everything that modifies a file or directory, or its security.
	sandboxDenyWriteMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES | fileDeleteChild | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER

	labelNoWriteUp = 0x1
	labelNoReadUp  = 0x2

	aceTypeAllowed = 0x0
	aceTypeDenied  = 0x1
	aceTypeLabel   = 0x11

	lowLabelSID    = "S-1-16-4096"
	mediumLabelSID = "S-1-16-8192"
)

func platformSandboxCommand(shell, command, workingDir string, policy sandboxPolicy) (string, []string, error) {
	if !policy.Network {
		return "", nil, fmt.Errorf("%s=off is not supported on Windows: the sandbox cannot block network access there", workspaceSandboxNetworkEnv)
	}
	name, sid, err := sandboxSID(policy.ProtectedRoot)
	if err != nil {
		return "", nil, err
	}

	// A private temp dir per workspace: labelling the user's whole %TEMP%
	// Low would open it to every sandbox and every Low process.
	userTemp := canonicalPath(os.TempDir())
	temp := filepath.Join(userTemp, "wingman-sandbox", name)
	if err := os.MkdirAll(temp, 0o700); err != nil {
		return "", nil, fmt.Errorf("create sandbox temp dir: %w", err)
	}
	roots := []string{temp}
	for _, root := range policy.WritableRoots {
		if !strings.EqualFold(root, userTemp) {
			roots = append(roots, root)
		}
	}
	for _, root := range roots {
		if err := ensureWritableRoot(root, sid); err != nil {
			return "", nil, err
		}
	}
	if err := ensureProtectedPaths(policy.ProtectedRoot, sid); err != nil {
		return "", nil, err
	}
	for _, path := range policy.DenyRead {
		if err := ensureLabel(path, mediumLabelSID, labelNoReadUp|labelNoWriteUp); err != nil {
			return "", nil, fmt.Errorf("protect credentials %q: %w", path, err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve sandbox helper: %w", err)
	}
	spec, err := json.Marshal(sandboxLaunch{
		SID:  sid.String(),
		Temp: temp,
		Argv: []string{shell, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", command},
	})
	if err != nil {
		return "", nil, err
	}
	return exe, []string{sandboxHelperArg, base64.RawURLEncoding.EncodeToString(spec)}, nil
}

// sandboxSID is stable per workspace so ACLs applied once keep working, and
// distinct across workspaces so one sandbox cannot write to another
// workspace's files. It lives under the resource manager authority (S-1-9),
// which no account or group uses.
func sandboxSID(workspace string) (string, *windows.SID, error) {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(workspace))))
	sid := "S-1-9"
	for i := range 4 {
		sid += fmt.Sprintf("-%d", binary.LittleEndian.Uint32(sum[i*4:]))
	}
	parsed, err := windows.StringToSid(sid)
	return "wingman-" + hex.EncodeToString(sum[:8]), parsed, err
}

func ensureWritableRoot(root string, sid *windows.SID) error {
	root = canonicalPath(root)
	home, _ := os.UserHomeDir()
	if strings.EqualFold(root, filepath.Dir(root)) || (home != "" && strings.EqualFold(root, canonicalPath(home))) {
		return fmt.Errorf("refusing to make %q writable for the sandbox: the Windows sandbox relabels writable roots, use a project directory", root)
	}
	if err := ensureACE(root, sid, windows.GRANT_ACCESS, sandboxRWMask, true); err != nil {
		return fmt.Errorf("grant sandbox access to %q: %w", root, err)
	}
	if err := ensureLabel(root, lowLabelSID, labelNoWriteUp); err != nil {
		return fmt.Errorf("label %q for the sandbox: %w", root, err)
	}
	return nil
}

// ensureProtectedPaths runs before every command: git and editors replace
// files like .git/config by rename, which drops an explicit deny ACE.
func ensureProtectedPaths(root string, sid *windows.SID) error {
	if root == "" {
		return nil
	}
	for _, path := range existingProtectedPaths(root) {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if err := ensureACE(path, sid, windows.DENY_ACCESS, sandboxDenyWriteMask, info.IsDir()); err != nil {
			return fmt.Errorf("protect %q: %w", path, err)
		}
	}
	// Renaming the workspace or .git away would let a command plant a fresh
	// .git with hooks; deny deleting them (not inherited by children).
	for _, path := range []string{root, filepath.Join(root, ".git")} {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		mask := windows.ACCESS_MASK(windows.DELETE)
		// A worktree stores its Git directory in a .git file. Changing that
		// pointer redirects Git to attacker-controlled configuration and hooks.
		if path != root && !info.IsDir() {
			mask = sandboxDenyWriteMask
		}
		if err := ensureACE(path, sid, windows.DENY_ACCESS, mask, false); err != nil {
			return fmt.Errorf("protect %q: %w", path, err)
		}
	}
	return nil
}

func ensureACE(path string, sid *windows.SID, mode windows.ACCESS_MODE, mask windows.ACCESS_MASK, inherit bool) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	aceType := byte(aceTypeAllowed)
	if mode == windows.DENY_ACCESS {
		aceType = aceTypeDenied
	}
	if hasACE(dacl, aceType, sid, uint32(mask), inherit) {
		return nil
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if inherit {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask,
		AccessMode:        mode,
		Inheritance:       inheritance,
		Trustee:           sidTrustee(sid),
	}}, dacl)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil)
}

func ensureLabel(path, labelSID string, policy uint32) error {
	want, err := windows.StringToSid(labelSID)
	if err != nil {
		return err
	}
	inherit := false
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		inherit = true
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if sacl, _, err := sd.SACL(); err == nil && hasACE(sacl, aceTypeLabel, want, policy, inherit) {
		return nil
	}
	flags := ""
	if inherit {
		flags = "OICI"
	}
	rights := "NW"
	if policy&labelNoReadUp != 0 {
		rights = "NRNW"
	}
	labelSD, err := windows.SecurityDescriptorFromString(fmt.Sprintf("S:(ML;%s;%s;;;%s)", flags, rights, labelSID))
	if err != nil {
		return err
	}
	sacl, _, err := labelSD.SACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, sacl)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	// Labelling needs WRITE_OWNER, which "Modify" ACLs (common on non-system
	// drives) withhold. The owner may always edit the DACL, so grant it.
	user, uerr := windows.GetCurrentProcessToken().GetTokenUser()
	if uerr != nil {
		return err
	}
	if uerr := ensureACE(path, user.User.Sid, windows.GRANT_ACCESS, windows.WRITE_OWNER, inherit); uerr != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, sacl)
}

// hasACE reports whether acl already carries an ACE of aceType for sid that
// covers mask (and, if inherit, propagates to children).
func hasACE(acl *windows.ACL, aceType byte, sid *windows.SID, mask uint32, inherit bool) bool {
	if acl == nil {
		return false
	}
	const inheritFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil {
			continue
		}
		if ace.Header.AceType != aceType || uint32(ace.Mask)&mask != mask {
			continue
		}
		if inherit && ace.Header.AceFlags&inheritFlags != inheritFlags {
			continue
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			return true
		}
	}
	return false
}

func sidTrustee(sid *windows.SID) windows.TRUSTEE {
	return windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
		TrusteeValue: windows.TrusteeValueFromSID(sid),
	}
}

func runSandboxHelper(encoded string) int {
	code, err := sandboxHelper(encoded)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wingman sandbox: %v\n", err)
		return 126
	}
	return int(code)
}

func sandboxHelper(encoded string) (uint32, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, err
	}
	var spec sandboxLaunch
	if err := json.Unmarshal(raw, &spec); err != nil {
		return 0, err
	}
	if len(spec.Argv) == 0 {
		return 0, errors.New("empty command")
	}
	sid, err := windows.StringToSid(spec.SID)
	if err != nil {
		return 0, err
	}

	// Pass only the helper's stdio to the child.
	var handles []windows.Handle
	stdio := [3]windows.Handle{}
	for i, id := range []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE} {
		h, err := windows.GetStdHandle(id)
		if err != nil || h == 0 || h == windows.InvalidHandle {
			continue
		}
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			continue
		}
		stdio[i] = h
		if !slices.Contains(handles, h) {
			handles = append(handles, h)
		}
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, err
	}
	defer attrs.Delete()
	if len(handles) > 0 {
		if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
			return 0, err
		}
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = stdio[0], stdio[1], stdio[2]

	appName, err := windows.UTF16PtrFromString(spec.Argv[0])
	if err != nil {
		return 0, err
	}
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(spec.Argv))
	if err != nil {
		return 0, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	dir, err := windows.UTF16PtrFromString(wd)
	if err != nil {
		return 0, err
	}
	env := sandboxEnvironmentBlock(os.Environ(), spec.Temp)

	token, err := restrictedSandboxToken(sid)
	if err != nil {
		return 0, err
	}
	defer token.Close()
	job, err := sandboxJob()
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(job)

	const flags = windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(token, appName, cmdline, nil, nil, true, flags, env, dir, &si.StartupInfo, &pi); err != nil {
		return 0, fmt.Errorf("start %s: %w", spec.Argv[0], err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)

	// The child must not run outside the job.
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		return 0, fmt.Errorf("assign sandbox job: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		return 0, err
	}
	if _, err := windows.WaitForSingleObject(pi.Process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return 0, err
	}
	// Background descendants die with the command, as they would through the
	// job handle if the helper itself is killed.
	windows.TerminateJobObject(job, 1)
	return code, nil
}

func sandboxJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{UIRestrictionsClass: windows.JOB_OBJECT_UILIMIT_HANDLES |
		windows.JOB_OBJECT_UILIMIT_READCLIPBOARD | windows.JOB_OBJECT_UILIMIT_WRITECLIPBOARD |
		windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS | windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS |
		windows.JOB_OBJECT_UILIMIT_DESKTOP | windows.JOB_OBJECT_UILIMIT_EXITWINDOWS}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectBasicUIRestrictions, uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

var procCreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")

// restrictedSandboxToken derives the sandbox token from the helper's own:
// no privileges beyond traversal, Administrators deny-only, Low integrity,
// and write-restricted to the workspace SID plus SIDs that objects meant for
// everyone grant (such as the NUL device and the desktop).
// Everyone is also required by CNG/KsecDD for PowerShell's random source.
// Consequently a pre-existing Low object granting Everyone or the logon SID
// write access remains writable; this is not a strict filesystem boundary.
func restrictedSandboxToken(sid *windows.SID) (windows.Token, error) {
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT, &self); err != nil {
		return 0, err
	}
	defer self.Close()

	restrict := []windows.SIDAndAttributes{{Sid: sid}}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinWorldSid, windows.WinRestrictedCodeSid} {
		s, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return 0, err
		}
		restrict = append(restrict, windows.SIDAndAttributes{Sid: s})
	}
	if groups, err := self.GetTokenGroups(); err == nil {
		for _, g := range groups.AllGroups() {
			if g.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
				restrict = append(restrict, windows.SIDAndAttributes{Sid: g.Sid})
			}
		}
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return 0, err
	}
	disable := []windows.SIDAndAttributes{{Sid: admins}}

	const disableMaxPrivilege, writeRestricted = 0x1, 0x8
	var token windows.Token
	if ok, _, err := procCreateRestrictedToken.Call(uintptr(self), disableMaxPrivilege|writeRestricted,
		uintptr(len(disable)), uintptr(unsafe.Pointer(&disable[0])), 0, 0,
		uintptr(len(restrict)), uintptr(unsafe.Pointer(&restrict[0])), uintptr(unsafe.Pointer(&token))); ok == 0 {
		return 0, fmt.Errorf("create restricted token: %w", err)
	}
	if err := configureSandboxToken(token, self, sid); err != nil {
		token.Close()
		return 0, err
	}
	return token, nil
}

func configureSandboxToken(token, self windows.Token, sid *windows.SID) error {
	low, err := windows.StringToSid(lowLabelSID)
	if err != nil {
		return err
	}
	label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: low, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(token, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)), label.Size()); err != nil {
		return fmt.Errorf("lower sandbox integrity: %w", err)
	}

	// Objects the shell creates without an inheritable DACL (named pipes,
	// events) must stay writable for it under the restricted check.
	user, err := self.GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, s := range []*windows.SID{user.User.Sid, system, sid} {
		entries = append(entries, windows.EXPLICIT_ACCESS{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS, Trustee: sidTrustee(s)})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	defaultDacl := struct{ DefaultDacl *windows.ACL }{dacl}
	if err := windows.SetTokenInformation(token, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&defaultDacl)), uint32(unsafe.Sizeof(defaultDacl))); err != nil {
		return fmt.Errorf("set sandbox default DACL: %w", err)
	}
	return nil
}

func sandboxEnvironmentBlock(environ []string, temp string) *uint16 {
	env := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.EqualFold(key, "TEMP") && !strings.EqualFold(key, "TMP") {
			env = append(env, kv)
		}
	}
	env = append(env, "TEMP="+temp, "TMP="+temp)
	sort.SliceStable(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })

	var block []uint16
	for _, kv := range env {
		if u, err := windows.UTF16FromString(kv); err == nil {
			block = append(block, u...)
		}
	}
	block = append(block, 0)
	return &block[0]
}
