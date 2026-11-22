package fs

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const readFlags = os.O_RDONLY

func removeEmptyDir(parent *os.File, name string) error {
	component, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	// Open a directory relative to the pinned parent. A reparse point or a
	// regular file at this name must survive directory pruning.
	attrs := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    component,
		Attributes:    windows.OBJ_DONT_REPARSE,
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var h windows.Handle
	err = windows.NtCreateFile(&h, windows.DELETE, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var remove uint32 = 1
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&remove)), uint32(unsafe.Sizeof(remove)))
}
