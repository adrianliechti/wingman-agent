//go:build windows

package shell

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The helper owns a native process security environment and a kill-on-close
// job. Filesystem policy belongs to the environment, never to host ACLs or
// integrity labels. There is deliberately no restricted-token/ACL fallback.
const sandboxHelperArg = "__wingman-sandbox-exec"

func init() {
	if len(os.Args) == 3 && os.Args[1] == sandboxHelperArg {
		os.Exit(runSandboxHelper(os.Args[2]))
	}
}

type sandboxLaunch struct {
	Policy sandboxPolicy `json:"policy"`
	Argv   []string      `json:"argv"`
}

func platformSandboxCommand(shell, command, workingDir string, policy sandboxPolicy) (string, []string, error) {
	if err := checkNativeSandboxSupport(); err != nil {
		return "", nil, err
	}
	for _, root := range policy.WritableRoots {
		if err := validateSandboxWritableRoot(root); err != nil {
			return "", nil, err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve sandbox helper: %w", err)
	}
	spec, err := json.Marshal(sandboxLaunch{
		Policy: policy,
		Argv:   []string{shell, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", command},
	})
	if err != nil {
		return "", nil, err
	}
	return exe, []string{sandboxHelperArg, base64.RawURLEncoding.EncodeToString(spec)}, nil
}

func validateSandboxWritableRoot(root string) error {
	root = canonicalPath(root)
	// Native Windows owns private temp storage. Ignore a legacy shared-temp
	// grant if an older caller supplied one; never make the host temp writable.
	if strings.EqualFold(root, canonicalPath(os.TempDir())) {
		return nil
	}
	home, _ := os.UserHomeDir()
	if !filepath.IsAbs(root) || strings.EqualFold(root, filepath.Dir(root)) || (home != "" && strings.EqualFold(root, canonicalPath(home))) {
		return fmt.Errorf("refusing to make %q writable for the sandbox: use a project directory, not a drive, share, or home root", root)
	}
	return nil
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
	if err := checkNativeSandboxSupport(); err != nil {
		return 0, err
	}
	for _, root := range spec.Policy.WritableRoots {
		if err := validateSandboxWritableRoot(root); err != nil {
			return 0, err
		}
	}

	// Windows provides each environment's private TEMP/TMP storage. Do not
	// grant the host's shared temp directory or create our own security state.
	policy, err := nativeSandboxPolicy(spec.Policy)
	if err != nil {
		return 0, err
	}
	environment, err := createNativeSandboxEnvironment(policy)
	if err != nil {
		return 0, err
	}
	defer closeNativeSandboxEnvironment(environment)

	// Only stdio is inherited; the environment and the job remain helper-owned.
	var handles []windows.Handle
	stdio := [3]windows.Handle{}
	for i, id := range []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE} {
		h, err := windows.GetStdHandle(id)
		if err != nil || h == 0 || h == windows.InvalidHandle {
			continue
		}
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return 0, fmt.Errorf("inherit sandbox stdio: %w", err)
		}
		stdio[i] = h
		if !slices.Contains(handles, h) {
			handles = append(handles, h)
		}
	}
	job, err := sandboxJob()
	if err != nil {
		return 0, err
	}
	// These defers run before releasing policy, including on launch failures.
	defer windows.CloseHandle(job)
	defer stopSandboxJob(job)
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return 0, err
	}
	defer attrs.Delete()
	const securityEnvironmentAttribute = 35 | 0x00020000
	if err := attrs.Update(securityEnvironmentAttribute, unsafe.Pointer(&environment), unsafe.Sizeof(environment)); err != nil {
		return 0, fmt.Errorf("attach native sandbox environment: %w", err)
	}
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
	env := sandboxEnvironmentBlock(os.Environ())

	const flags = windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW
	var pi windows.ProcessInformation
	err = windows.CreateProcess(appName, cmdline, nil, nil, len(handles) > 0, flags, env, dir, &si.StartupInfo, &pi)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(env)
	runtime.KeepAlive(environment)
	if err != nil {
		return 0, fmt.Errorf("start %s in native sandbox: %w", spec.Argv[0], err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	// A suspended child can never execute without both policy and the job.
	// PSEC does not preserve the job-list startup attribute on all hosts; use
	// explicit assignment before resuming, as MXC's native runner does.
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		return 0, fmt.Errorf("assign sandbox job: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		return 0, err
	}
	if _, err := windows.WaitForSingleObject(pi.Process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return 0, err
	}
	if err := stopSandboxJob(job); err != nil {
		return 0, err
	}
	return code, nil
}

const sandboxUIRestrictions = windows.JOB_OBJECT_UILIMIT_READCLIPBOARD | windows.JOB_OBJECT_UILIMIT_WRITECLIPBOARD |
	windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS | windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS |
	windows.JOB_OBJECT_UILIMIT_DESKTOP | windows.JOB_OBJECT_UILIMIT_EXITWINDOWS | 0x100 | 0x200 // IME, input injection

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
	ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{UIRestrictionsClass: sandboxUIRestrictions}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectBasicUIRestrictions, uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func stopSandboxJob(job windows.Handle) error {
	if err := windows.TerminateJobObject(job, 1); err != nil {
		return fmt.Errorf("terminate sandbox job: %w", err)
	}
	// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION is fixed-size, also on 32-bit.
	var accounting struct {
		TotalUserTime, TotalKernelTime, PeriodUserTime, PeriodKernelTime int64
		PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return fmt.Errorf("reap sandbox job: %w", err)
		}
		if accounting.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("sandbox process tree did not stop within 5 seconds")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sandboxEnvironmentBlock(environ []string) *uint16 {
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.EqualFold(key, workspaceSandboxActiveEnv) {
			env = append(env, kv)
		}
	}
	env = append(env, workspaceSandboxActiveEnv+"=1")
	slices.SortFunc(env, func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) })
	var block []uint16
	for _, kv := range env {
		if u, err := windows.UTF16FromString(kv); err == nil {
			block = append(block, u...)
		}
	}
	block = append(block, 0)
	return &block[0]
}
