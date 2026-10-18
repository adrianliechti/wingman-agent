//go:build windows

package shell

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Building a native process security environment takes seconds, and its
// handle is only valid in the process that created it. This process therefore
// keeps one environment per distinct policy and creates every sandboxed shell
// itself. A per-command helper keeps exec.Cmd semantics: it owns stdio and a
// kill-on-close job, and requests its shell over a private pipe. The shell is
// created suspended with the helper's stdio, assigned to the helper's job, and
// only then resumed. Filesystem policy belongs to the
// environment, never to host ACLs or integrity labels. There is deliberately
// no restricted-token/ACL fallback.
const sandboxHelperArg = "__wingman-sandbox-exec"

func init() {
	if len(os.Args) == 3 && os.Args[1] == sandboxHelperArg {
		os.Exit(runSandboxHelper(os.Args[2]))
	}
}

type sandboxLaunch struct {
	Pipe         string        `json:"pipe"`
	Policy       sandboxPolicy `json:"policy"`
	Argv         []string      `json:"argv"`
	StderrMarker string        `json:"stderr_marker"`
}

// sandboxLaunchRequest carries handle values valid in the helper process.
type sandboxLaunchRequest struct {
	Policy sandboxPolicy     `json:"policy"`
	Argv   []string          `json:"argv"`
	Dir    string            `json:"dir"`
	Env    []string          `json:"env"`
	Stdio  [3]windows.Handle `json:"stdio"`
	Job    windows.Handle    `json:"job"`
}

type sandboxLaunchReply struct {
	Process windows.Handle `json:"process"`
	Error   string         `json:"error,omitempty"`
}

func platformSandboxCommand(shell, command, workingDir string, policy sandboxPolicy) (string, []string, error) {
	if err := platformSandboxCheck("", policy); err != nil {
		return "", nil, err
	}
	pipe, err := sandboxLauncherPipe()
	if err != nil {
		return "", nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve sandbox helper: %w", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, err
	}
	marker := fmt.Sprintf("__wingman_sandbox_started_%x__", nonce)
	// Provider initialization precedes -Command. End startup filtering before
	// any user code runs, including commands that print the same diagnostic.
	command = "[Console]::Error.WriteLine('" + marker + "'); " + command
	spec, err := json.Marshal(sandboxLaunch{
		Pipe:         pipe,
		Policy:       policy,
		Argv:         []string{shell, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", command},
		StderrMarker: marker,
	})
	if err != nil {
		return "", nil, err
	}
	return exe, []string{sandboxHelperArg, base64.RawURLEncoding.EncodeToString(spec)}, nil
}

// platformSandboxCheck only probes support; environments are created on
// first use.
func platformSandboxCheck(workspace string, policy sandboxPolicy) error {
	if err := checkNativeSandboxSupport(); err != nil {
		return err
	}
	for _, root := range policy.WritableRoots {
		if err := validateSandboxWritableRoot(root); err != nil {
			return err
		}
	}
	if workspace != "" {
		// Session startup warms the volume reads before the first command.
		go sandboxVolumeReads()
	}
	return nil
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

var sandboxLauncher struct {
	once sync.Once
	pipe string
	err  error
}

// sandboxLauncherPipe starts serving launch requests on a pipe that only
// this user's unsandboxed processes can open: an AppContainer token also
// needs a grant for its package SID, so sandboxed shells cannot connect.
func sandboxLauncherPipe() (string, error) {
	sandboxLauncher.once.Do(func() {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			sandboxLauncher.err = err
			return
		}
		sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.User.Sid.String() + ")")
		if err != nil {
			sandboxLauncher.err = err
			return
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			sandboxLauncher.err = err
			return
		}
		name := fmt.Sprintf(`\\.\pipe\wingman-sandbox-%d-%x`, os.Getpid(), nonce)
		first, err := createSandboxLauncherPipe(name, sd, true)
		if err != nil {
			sandboxLauncher.err = err
			return
		}
		sandboxLauncher.pipe = name
		go serveSandboxLauncher(name, sd, first)
	})
	return sandboxLauncher.pipe, sandboxLauncher.err
}

func createSandboxLauncherPipe(name string, sd *windows.SECURITY_DESCRIPTOR, first bool) (windows.Handle, error) {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	flags := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	const mode = windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS
	h, err := windows.CreateNamedPipe(path, flags, mode, windows.PIPE_UNLIMITED_INSTANCES, 4096, 4096, 0, &sa)
	if err != nil {
		return 0, fmt.Errorf("create sandbox launcher pipe: %w", err)
	}
	return h, nil
}

func serveSandboxLauncher(name string, sd *windows.SECURITY_DESCRIPTOR, next windows.Handle) {
	for {
		pipe := next
		if err := windows.ConnectNamedPipe(pipe, nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			windows.CloseHandle(pipe)
			time.Sleep(100 * time.Millisecond)
		} else {
			go handleSandboxLaunch(pipe)
		}
		for {
			var err error
			if next, err = createSandboxLauncherPipe(name, sd, false); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
}

func handleSandboxLaunch(pipe windows.Handle) {
	var helperPID uint32
	pidErr := windows.GetNamedPipeClientProcessId(pipe, &helperPID)
	conn := os.NewFile(uintptr(pipe), "sandbox-launcher")
	defer conn.Close()
	var request sandboxLaunchRequest
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		return
	}
	var reply sandboxLaunchReply
	if pidErr != nil {
		reply.Error = fmt.Sprintf("identify sandbox helper: %v", pidErr)
	} else if process, err := launchSandboxed(helperPID, request); err != nil {
		reply.Error = err.Error()
	} else {
		reply.Process = process
	}
	json.NewEncoder(conn).Encode(reply)
}

// launchSandboxed starts the requested shell for the helper and returns the
// helper's handle to it. The shell runs only once it is in the helper's job.
func launchSandboxed(helperPID uint32, request sandboxLaunchRequest) (windows.Handle, error) {
	if len(request.Argv) == 0 {
		return 0, errors.New("empty command")
	}
	if err := platformSandboxCheck("", request.Policy); err != nil {
		return 0, err
	}
	policy, err := nativeSandboxPolicy(request.Policy)
	if err != nil {
		return 0, err
	}
	helper, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE|windows.SYNCHRONIZE, false, helperPID)
	if err != nil {
		return 0, fmt.Errorf("open sandbox helper: %w", err)
	}
	tracking := false
	defer func() {
		if !tracking {
			windows.CloseHandle(helper)
		}
	}()
	const jobObjectAssignProcess = 0x0001
	const jobObjectQuery = 0x0004
	const jobObjectTerminate = 0x0008
	var job windows.Handle
	if err := windows.DuplicateHandle(helper, request.Job, windows.CurrentProcess(), &job, jobObjectAssignProcess|jobObjectQuery|jobObjectTerminate, false, 0); err != nil {
		return 0, fmt.Errorf("open sandbox job: %w", err)
	}
	defer func() {
		if !tracking {
			windows.CloseHandle(job)
		}
	}()

	// Borrow the helper's stdio; only these handles are inherited.
	var stdio [3]windows.Handle
	borrowed := map[windows.Handle]windows.Handle{}
	defer func() {
		for _, h := range borrowed {
			windows.CloseHandle(h)
		}
	}()
	for i, remote := range request.Stdio {
		if remote == 0 {
			continue
		}
		local, ok := borrowed[remote]
		if !ok {
			if err := windows.DuplicateHandle(helper, remote, windows.CurrentProcess(), &local, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
				return 0, fmt.Errorf("borrow sandbox stdio: %w", err)
			}
			borrowed[remote] = local
		}
		stdio[i] = local
	}

	environment, release, err := acquireNativeSandboxEnvironment(policy)
	if err != nil {
		// Cached volume reads can name paths that have since vanished or
		// become unreadable. Current hosts accept such read grants, but a
		// host that validates them would fail here: rescan once and retry.
		if !invalidateSandboxVolumeReads() {
			return 0, err
		}
		if policy, err = nativeSandboxPolicy(request.Policy); err != nil {
			return 0, err
		}
		if environment, release, err = acquireNativeSandboxEnvironment(policy); err != nil {
			return 0, err
		}
	}
	defer func() {
		if !tracking {
			release()
		}
	}()
	pi, err := createSandboxedProcess(environment, stdio, request)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	defer func() {
		if !tracking {
			windows.TerminateProcess(pi.Process, 1)
			windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		}
	}()
	// A suspended child can never execute without both policy and the job.
	// PSEC does not preserve the job-list startup attribute on all hosts; use
	// explicit assignment before resuming, as MXC's native runner does.
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		return 0, fmt.Errorf("assign sandbox job: %w", err)
	}
	var process windows.Handle
	const access = windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION
	if err := windows.DuplicateHandle(windows.CurrentProcess(), pi.Process, helper, &process, access, false, 0); err != nil {
		return 0, fmt.Errorf("hand sandboxed process to helper: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		return 0, err
	}
	tracking = true
	go func() {
		defer windows.CloseHandle(helper)
		defer windows.CloseHandle(job)
		// Our job handle keeps the job alive after a forced helper kill,
		// so explicitly stop it when the helper exits. Keep the environment
		// until every child is gone, even if the main shell exited earlier.
		if _, err := windows.WaitForSingleObject(helper, windows.INFINITE); err == nil {
			if stopSandboxJob(job) == nil {
				release()
			}
		}
	}()
	return process, nil
}

func createSandboxedProcess(environment windows.Handle, stdio [3]windows.Handle, request sandboxLaunchRequest) (*windows.ProcessInformation, error) {
	var handles []windows.Handle
	for _, h := range stdio {
		if h != 0 && !slices.Contains(handles, h) {
			handles = append(handles, h)
		}
	}
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	const securityEnvironmentAttribute = 35 | 0x00020000
	if err := attrs.Update(securityEnvironmentAttribute, unsafe.Pointer(&environment), unsafe.Sizeof(environment)); err != nil {
		return nil, fmt.Errorf("attach native sandbox environment: %w", err)
	}
	if len(handles) > 0 {
		if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
			return nil, err
		}
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = stdio[0], stdio[1], stdio[2]
	appName, err := windows.UTF16PtrFromString(request.Argv[0])
	if err != nil {
		return nil, err
	}
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(request.Argv))
	if err != nil {
		return nil, err
	}
	dir, err := windows.UTF16PtrFromString(request.Dir)
	if err != nil {
		return nil, err
	}
	env := sandboxEnvironmentBlock(request.Env)

	const flags = windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW
	var pi windows.ProcessInformation
	err = windows.CreateProcess(appName, cmdline, nil, nil, len(handles) > 0, flags, env, dir, &si.StartupInfo, &pi)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(env)
	runtime.KeepAlive(environment)
	if err != nil {
		return nil, fmt.Errorf("start %s in native sandbox: %w", request.Argv[0], err)
	}
	return &pi, nil
}

type cachedSandboxEnvironment struct {
	key      string
	handle   windows.Handle
	active   int
	lastUsed time.Time
}

// Idle environments beyond this many are closed. One with live processes is
// kept: policy enforcement must outlive the processes that rely on it.
const maxSandboxEnvironments = 4

var sandboxEnvironments struct {
	sync.Mutex
	cached []*cachedSandboxEnvironment
}

// acquireNativeSandboxEnvironment retains an environment until release is
// called after a failed launch or after the command's entire job has stopped.
func acquireNativeSandboxEnvironment(policy nativeFilesystemPolicy) (windows.Handle, func(), error) {
	key, err := json.Marshal(policy)
	if err != nil {
		return 0, nil, err
	}
	sandboxEnvironments.Lock()
	defer sandboxEnvironments.Unlock()
	var entry *cachedSandboxEnvironment
	for _, candidate := range sandboxEnvironments.cached {
		if candidate.key == string(key) {
			entry = candidate
			break
		}
	}
	if entry == nil {
		handle, err := createNativeSandboxEnvironment(policy)
		if err != nil {
			return 0, nil, err
		}
		entry = &cachedSandboxEnvironment{key: string(key), handle: handle}
		sandboxEnvironments.cached = append(sandboxEnvironments.cached, entry)
	}
	entry.lastUsed = time.Now()
	entry.active++

	slices.SortFunc(sandboxEnvironments.cached, func(a, b *cachedSandboxEnvironment) int { return b.lastUsed.Compare(a.lastUsed) })
	kept := sandboxEnvironments.cached[:0]
	for i, candidate := range sandboxEnvironments.cached {
		if i >= maxSandboxEnvironments && candidate.active == 0 {
			closeNativeSandboxEnvironment(candidate.handle)
			continue
		}
		kept = append(kept, candidate)
	}
	sandboxEnvironments.cached = kept

	release := func() {
		sandboxEnvironments.Lock()
		defer sandboxEnvironments.Unlock()
		entry.active--
	}
	return entry.handle, release, nil
}

func runSandboxHelper(encoded string) int {
	code, err := sandboxHelper(encoded)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wingman sandbox: %v\n", err)
		return 126
	}
	return int(code)
}

func sandboxHelper(encoded string) (code uint32, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, err
	}
	var spec sandboxLaunch
	if err := json.Unmarshal(raw, &spec); err != nil {
		return 0, err
	}
	request := sandboxLaunchRequest{Policy: spec.Policy, Argv: spec.Argv, Env: sandboxEnviron(os.Environ())}
	if request.Dir, err = os.Getwd(); err != nil {
		return 0, err
	}
	// The shell gets only this stdio and starts only inside this job.
	for i, id := range []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE} {
		if h, err := windows.GetStdHandle(id); err == nil && h != windows.InvalidHandle {
			request.Stdio[i] = h
		}
	}
	job, err := sandboxJob()
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(job)
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("create sandbox stderr pipe: %w", err)
	}
	defer stderrRead.Close()
	request.Stdio[2] = windows.Handle(stderrWrite.Fd())
	stderrDone := make(chan error, 1)
	go func() {
		stderrDone <- copyPowerShellStartupStderr(os.Stderr, stderrRead, spec.StderrMarker)
	}()
	defer func() {
		stderrWrite.Close()
		select {
		case copyErr := <-stderrDone:
			if err == nil && copyErr != nil {
				err = fmt.Errorf("forward sandbox stderr: %w", copyErr)
			}
		case <-time.After(2 * time.Second):
			// Bound cleanup even when a failed job teardown leaves inherited
			// writers open, as exec.Cmd.WaitDelay does for its output pipes.
			if err == nil {
				err = errors.New("sandbox stderr did not close within 2 seconds")
			}
		}
	}()
	// Stop descendants before draining stderr: they may inherit its writer.
	defer func() {
		if stopErr := stopSandboxJob(job); err == nil {
			err = stopErr
		}
	}()
	request.Job = job

	reply, err := requestSandboxLaunch(spec.Pipe, request)
	stderrWrite.Close() // The shell owns the remaining writer after launch.
	if err != nil {
		return 0, err
	}
	process := reply.Process
	defer windows.CloseHandle(process)
	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		return 0, err
	}
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return 0, err
	}
	return code, nil
}

func requestSandboxLaunch(pipe string, request sandboxLaunchRequest) (sandboxLaunchReply, error) {
	var reply sandboxLaunchReply
	// Instances are created one at a time; retry briefly while all are busy.
	var conn *os.File
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if conn, err = os.OpenFile(pipe, os.O_RDWR, 0); !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return reply, fmt.Errorf("connect to sandbox launcher: %w", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return reply, fmt.Errorf("request sandbox launch: %w", err)
	}
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return reply, fmt.Errorf("request sandbox launch: %w", err)
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
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

// sandboxEnviron marks the environment as sandboxed.
func sandboxEnviron(environ []string) []string {
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.EqualFold(key, workspaceSandboxActiveEnv) {
			env = append(env, kv)
		}
	}
	return append(env, workspaceSandboxActiveEnv+"=1")
}

func sandboxEnvironmentBlock(env []string) *uint16 {
	env = slices.Clone(env)
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
