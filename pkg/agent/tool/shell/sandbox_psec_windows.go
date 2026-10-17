//go:build windows

package shell

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	flatbuffers "github.com/google/flatbuffers/go"
	"golang.org/x/sys/windows"
)

var (
	processModel = windows.NewLazySystemDLL("processmodel.dll")
	psecCreate   = processModel.NewProc("CreateProcessSecurityEnvironment")
	psecClose    = processModel.NewProc("CloseProcessSecurityEnvironment")
	psecQuery    = processModel.NewProc("QueryProcessSecurityEnvironmentSupport")
	psecVersion  = processModel.NewProc("IsProcessSecurityEnvironmentVersionSupported")
	apiSetCheck  = windows.NewLazySystemDLL("kernelbase.dll").NewProc("IsApiSetImplemented")
)

var checkNativeSandboxSupport = sync.OnceValue(func() error {
	if err := apiSetCheck.Find(); err != nil {
		return nativeSandboxUnavailable(err)
	}
	name, _ := windows.BytePtrFromString("api-win-appmodel-processmodel~securityenvironment")
	present, _, _ := apiSetCheck.Call(uintptr(unsafe.Pointer(name)))
	if present == 0 {
		return nativeSandboxUnavailable(fmt.Errorf("process security environment API set is absent"))
	}
	for _, proc := range []*windows.LazyProc{psecCreate, psecClose, psecQuery, psecVersion} {
		if err := proc.Find(); err != nil {
			return nativeSandboxUnavailable(err)
		}
	}
	var available byte
	var minor uint32
	hr, _, _ := psecVersion.Call(1, uintptr(unsafe.Pointer(&available)), uintptr(unsafe.Pointer(&minor)))
	if int32(hr) < 0 || available == 0 {
		return nativeSandboxUnavailable(fmt.Errorf("PSEC 1.0 is unavailable (HRESULT 0x%08x)", uint32(hr)))
	}
	var flags uint64
	hr, _, _ = psecQuery.Call(uintptr(unsafe.Pointer(&flags)))
	if int32(hr) < 0 || flags&1 == 0 {
		return nativeSandboxUnavailable(fmt.Errorf("native denied paths are unavailable (HRESULT 0x%08x)", uint32(hr)))
	}
	return nil
})

func nativeSandboxUnavailable(err error) error {
	return fmt.Errorf("native Windows sandbox is unavailable: %w; update Windows to a build with PSEC 1.0 and native denied paths", err)
}

type nativeFilesystemPolicy struct {
	ReadWrite []string
	ReadOnly  []string
	Deny      []string
	Network   bool
}

func nativeSandboxPolicy(policy sandboxPolicy) (nativeFilesystemPolicy, error) {
	result := nativeFilesystemPolicy{Network: policy.Network}
	add := func(paths *[]string, path string) {
		path = canonicalPath(path)
		for _, existing := range *paths {
			if strings.EqualFold(path, existing) {
				return
			}
		}
		*paths = append(*paths, path)
	}
	for _, path := range policy.WritableRoots {
		if !strings.EqualFold(canonicalPath(path), canonicalPath(os.TempDir())) {
			add(&result.ReadWrite, path)
		}
	}
	for _, path := range policy.DenyRead {
		add(&result.Deny, path)
	}
	// PSEC resolves junctions, but an existing hardlink can give a writable
	// pathname to an outside (or credential) file. The API blocks creation of
	// new outside links, not writes through links already in the workspace.
	// Deny multiply linked files in writable trees before the workload runs.
	// This conservative rule avoids granting a credential read via an alias
	// without scanning other volumes or modifying the underlying file.
	for _, root := range result.ReadWrite {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			h, err := openSandboxPolicyPath(path)
			if err != nil {
				return err
			}
			var info windows.ByHandleFileInformation
			err = windows.GetFileInformationByHandle(h, &info)
			windows.CloseHandle(h)
			if err != nil {
				return err
			}
			if info.NumberOfLinks > 1 {
				add(&result.Deny, path)
			}
			return nil
		}); err != nil {
			return result, fmt.Errorf("check sandbox hardlinks beneath %q: %w", root, err)
		}
	}
	for _, root := range sandboxProtectedRoots(policy) {
		for _, path := range existingProtectedPaths(root) {
			add(&result.ReadOnly, path)
		}
	}
	carveouts := append([]string(nil), result.ReadOnly...)

	// PSEC volume-root grants are nonrecursive. As in Codex's native adapter,
	// expand currently accessible immediate children to retain broad reads.
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return result, fmt.Errorf("enumerate sandbox read volumes: %w", err)
	}
	for i := range 26 {
		if drives&(1<<i) == 0 {
			continue
		}
		root := fmt.Sprintf("%c:\\", 'A'+i)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // Empty/removable/disconnected volumes are not required.
		}
		add(&result.ReadOnly, root)
		for _, entry := range entries {
			path := filepath.Join(root, entry.Name())
			// The OS rejects grants for inaccessible objects. Only implicit read
			// grants may be skipped; explicit writes and denies fail at creation.
			h, err := openSandboxPolicyPath(path)
			if err != nil {
				continue
			}
			windows.CloseHandle(h)
			add(&result.ReadOnly, path)
		}
	}
	// Resolve equal-path access modes before serializing. Narrower read-only
	// and deny paths remain carveouts beneath writable roots.
	contains := func(paths []string, path string) bool {
		for _, other := range paths {
			if strings.EqualFold(other, path) {
				return true
			}
		}
		return false
	}
	var writes []string
	for _, path := range result.ReadWrite {
		if !contains(carveouts, path) && !contains(result.Deny, path) {
			writes = append(writes, path)
		}
	}
	result.ReadWrite = writes
	var reads []string
	for _, path := range result.ReadOnly {
		if !contains(result.ReadWrite, path) && !contains(result.Deny, path) {
			reads = append(reads, path)
		}
	}
	result.ReadOnly = reads
	return result, nil
}

func openSandboxPolicyPath(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

func createNativeSandboxEnvironment(policy nativeFilesystemPolicy) (windows.Handle, error) {
	buf := encodeNativeSandboxPolicy(policy)
	var environment windows.Handle
	hr, _, _ := psecCreate.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, uintptr(unsafe.Pointer(&environment)))
	runtime.KeepAlive(buf)
	if int32(hr) < 0 {
		return 0, fmt.Errorf("create native Windows sandbox: HRESULT 0x%08x", uint32(hr))
	}
	if environment == 0 || environment == windows.InvalidHandle {
		return 0, fmt.Errorf("create native Windows sandbox returned an invalid environment")
	}
	return environment, nil
}

func closeNativeSandboxEnvironment(environment windows.Handle) {
	psecClose.Call(uintptr(environment))
}

// Encode the PSEC 1.0 subset of the pinned Microsoft schema in psec/.
// Slots and defaults come directly from that schema. Version 1.1 additions
// (fs_enumerate and ingress) are deliberately omitted for 1.0 hosts.
// This small writer uses the pinned FlatBuffers runtime, with OS parsing and
// enforcement tested end to end; it does not implement its own wire format.
func encodeNativeSandboxPolicy(policy nativeFilesystemPolicy) []byte {
	b := flatbuffers.NewBuilder(1024)
	vector := func(paths []string) flatbuffers.UOffsetT {
		if len(paths) == 0 {
			return 0
		}
		offsets := make([]flatbuffers.UOffsetT, len(paths))
		for i, path := range paths {
			offsets[i] = b.CreateString(path)
		}
		b.StartVector(4, len(paths), 4)
		for i := len(offsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(offsets[i])
		}
		return b.EndVector(len(paths))
	}
	rw, ro, deny := vector(policy.ReadWrite), vector(policy.ReadOnly), vector(policy.Deny)
	capabilities := "registryRead"
	var loopback flatbuffers.UOffsetT
	if policy.Network {
		capabilities += ",internetClient,privateNetworkClientServer,networkLoopback"
		loopback = b.CreateString("MXC-Loopback")
	}
	caps := b.CreateString(capabilities)
	b.StartObject(3) // EndpointPolicy: default_action, allow, deny
	if policy.Network {
		b.PrependInt8Slot(0, 1, 0) // FilterAction.allow
	}
	egress := b.EndObject()
	b.StartObject(4) // NetworkPolicy: proxy, egress, peer, ingress
	b.PrependUOffsetTSlot(1, egress, 0)
	b.PrependUOffsetTSlot(2, loopback, 0)
	network := b.EndObject()
	b.StartObject(9) // ProcessSecurityEnvironment; append-only schema slots
	b.PrependUOffsetTSlot(1, caps, 0)
	b.PrependUint64Slot(3, uint64(sandboxUIRestrictions), 0)
	b.PrependUOffsetTSlot(4, rw, 0)
	b.PrependUOffsetTSlot(5, ro, 0)
	b.PrependUOffsetTSlot(6, deny, 0)
	b.PrependUOffsetTSlot(7, network, 0)
	// SchemaVersion is an inline struct { major:uint16, minor:uint16 }.
	b.Prep(2, 4)
	b.PrependUint16(0)
	b.PrependUint16(1)
	b.PrependStructSlot(0, b.Offset(), 0)
	root := b.EndObject()
	b.FinishWithFileIdentifier(root, []byte("PSEC"))
	return b.FinishedBytes()
}
