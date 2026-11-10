//go:build darwin

package shell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fcntlMakeCompressed  = 80  // F_MAKECOMPRESSED
	fcntlTransferExtents = 110 // F_TRANSFEREXTENTS

	// fcntlProbeEnv lists the canary, donor and receiver files for the probe.
	fcntlProbeEnv = "WINGMAN_FCNTL_PROBE"
)

// Both fcntls change a file through a descriptor opened read-only, so the
// write rules do not stop them. Ported from Codex's seatbelt_fcntl_tests.rs.
func TestSandboxBlocksMutatingFcntls(t *testing.T) {
	f := newEscapeFixture(t)
	probe := func(canary, donor, receiver string) string {
		return fcntlProbeEnv + "=" + escapeQuote(strings.Join([]string{canary, donor, receiver}, string(filepath.ListSeparator))) +
			" " + escapeQuote(os.Args[0]) + " -test.run='^TestSandboxFcntlProbe$' -test.v"
	}
	fixture := func(outside, workspace string) (canary, donor, receiver string) {
		canary, donor, receiver = filepath.Join(outside, "canary"), filepath.Join(workspace, "donor"), filepath.Join(outside, "receiver")
		writeFixture(t, canary, "outside sandbox canary\n")
		writeFixture(t, donor, strings.Repeat("d", 65536))
		writeFixture(t, receiver, "")
		return canary, donor, receiver
	}
	compressed := func(path string) bool {
		var stat unix.Stat_t
		if err := unix.Stat(path, &stat); err != nil {
			t.Fatal(err)
		}
		return stat.Flags&unix.UF_COMPRESSED != 0
	}

	// Without the sandbox the probe has to change the canary, or the denial
	// below proves nothing about the profile.
	canary, donor, receiver := fixture(t.TempDir(), t.TempDir())
	control, err := exec.Command("/bin/sh", "-c", probe(canary, donor, receiver)).CombinedOutput()
	if err != nil || !compressed(canary) {
		t.Skipf("F_MAKECOMPRESSED has no effect here, so the denial cannot be checked: %v\n%s", err, control)
	}
	if strings.Contains(string(control), "F_TRANSFEREXTENTS: denied") {
		t.Skipf("F_TRANSFEREXTENTS is refused without a sandbox, so the denial cannot be checked:\n%s", control)
	}

	canary, donor, receiver = fixture(f.outside, f.workspace)
	before := treeDigest(t, f.outside, donor)
	output, _ := runSandboxed(t, f.workspace, probe(canary, donor, receiver), nil)
	for _, want := range []string{"F_MAKECOMPRESSED: denied", "F_TRANSFEREXTENTS: denied"} {
		if !strings.Contains(output, want) {
			t.Errorf("sandbox did not deny the operation, want %q in:\n%s", want, output)
		}
	}
	if compressed(canary) || treeDigest(t, f.outside, donor) != before {
		t.Error("a sandboxed command changed a file through a read-only descriptor")
	}
}

// TestSandboxFcntlProbe is the child process of TestSandboxBlocksMutatingFcntls.
func TestSandboxFcntlProbe(t *testing.T) {
	paths := filepath.SplitList(os.Getenv(fcntlProbeEnv))
	if len(paths) != 3 {
		t.Skip("runs only as the child of TestSandboxBlocksMutatingFcntls")
	}
	var files [3]*os.File
	for i, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		files[i] = file
	}
	canary, donor, receiver := files[0], files[1], files[2]

	// F_MAKECOMPRESSED acts only when given the file's current generation.
	list := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_GEN_COUNT}
	var reply struct{ length, generation uint32 }
	if _, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, canary.Fd(), uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&reply)), unsafe.Sizeof(reply), unix.FSOPT_ATTR_CMN_EXTENDED, 0); errno != 0 {
		t.Fatalf("read file generation: %v", errno)
	}

	report := func(name string, err error) {
		switch {
		case err == nil:
			fmt.Printf("%s: ok\n", name)
		case errors.Is(err, unix.EPERM):
			fmt.Printf("%s: denied\n", name)
		default:
			fmt.Printf("%s: %v\n", name, err)
		}
	}
	_, err := unix.FcntlInt(canary.Fd(), fcntlMakeCompressed, int(reply.generation))
	report("F_MAKECOMPRESSED", err)
	_, err = unix.FcntlInt(donor.Fd(), fcntlTransferExtents, int(receiver.Fd()))
	report("F_TRANSFEREXTENTS", err)
}
