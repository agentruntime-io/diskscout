package main

// The few Win32 calls the app needs. Everything else goes through the standard library.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// Attributes we never follow or read: reparse points (symlinks, junctions, OneDrive placeholders), offline files,
// and anything that would be pulled down from the cloud when opened.
const skipAttrs = 0x400 | 0x1000 | 0x40000 | 0x400000

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	shell32             = syscall.NewLazyDLL("shell32.dll")
	pGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
	pGetLogicalDrives   = kernel32.NewProc("GetLogicalDrives")
	pGetDriveType       = kernel32.NewProc("GetDriveTypeW")
	pSetPriorityClass   = kernel32.NewProc("SetPriorityClass")
	pGetCurrentProcess  = kernel32.NewProc("GetCurrentProcess")
	pSHFileOperation    = shell32.NewProc("SHFileOperationW")
	pSHEmptyRecycleBin  = shell32.NewProc("SHEmptyRecycleBinW")
)

// fileAttrs returns the Windows attribute bits of a directory entry, which came for free with the listing.
func fileAttrs(info os.FileInfo) uint32 {
	if d, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes
	}
	return 0
}

func diskUsage(root string) (free, total int64, err error) {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, err
	}
	var f, t, tf uint64
	r, _, e := pGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&f)),
		uintptr(unsafe.Pointer(&t)), uintptr(unsafe.Pointer(&tf)))
	if r == 0 {
		return 0, 0, e
	}
	return int64(f), int64(t), nil
}

// fixedDrives lists the roots of local fixed disks, e.g. ["C:\\", "D:\\"].
func fixedDrives() []string {
	mask, _, _ := pGetLogicalDrives.Call()
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := syscall.UTF16PtrFromString(root)
		if t, _, _ := pGetDriveType.Call(uintptr(unsafe.Pointer(p))); t == 3 { // DRIVE_FIXED
			out = append(out, root)
		}
	}
	return out
}

// lowerPriority makes the scan polite: the machine stays usable while a whole disk is being read.
func lowerPriority() {
	h, _, _ := pGetCurrentProcess.Call()
	pSetPriorityClass.Call(h, 0x4000) // BELOW_NORMAL_PRIORITY_CLASS
}

func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) || !filepath.IsAbs(p) {
		return p
	}
	p = filepath.Clean(p)
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

type fileKey struct{ vol, hi, lo uint32 }

// fileIdentity returns the on-disk identity of a file (same volume + index = same physical file) and its link count.
func fileIdentity(path string) (fileKey, uint32, error) {
	p, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return fileKey{}, 0, err
	}
	const readAttributes = 0x80
	h, err := syscall.CreateFile(p, readAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fileKey{}, 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.CloseHandle(h)
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &d); err != nil {
		return fileKey{}, 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return fileKey{d.VolumeSerialNumber, d.FileIndexHigh, d.FileIndexLow}, d.NumberOfLinks, nil
}

type shFileOp struct {
	Hwnd         uintptr
	Func         uint32
	From         *uint16
	To           *uint16
	Flags        uint16
	Aborted      int32
	NameMappings uintptr
	Title        *uint16
}

// moveToRecycleBin sends one file or folder to the Recycle Bin. WANTNUKEWARNING keeps Windows' own prompt for the
// case where the item is too big for the bin and would be destroyed instead of recycled.
func moveToRecycleBin(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	from, err := syscall.UTF16FromString(abs)
	if err != nil {
		return err
	}
	from = append(from, 0) // the list is double-NUL terminated
	const (
		foDelete        = 3
		fofSilent       = 0x4
		fofNoConfirm    = 0x10
		fofAllowUndo    = 0x40
		fofNoErrorUI    = 0x400
		fofWantNukeWarn = 0x4000
	)
	op := shFileOp{Func: foDelete, From: &from[0], Flags: fofSilent | fofNoConfirm | fofAllowUndo | fofNoErrorUI | fofWantNukeWarn}
	r, _, _ := pSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(from)
	if r != 0 {
		return fmt.Errorf("the Recycle Bin refused it (code 0x%X)", r)
	}
	if op.Aborted != 0 {
		return errors.New("cancelled")
	}
	return nil
}

// emptyRecycleBin empties the bin of one drive without Windows' own prompts: the app asks first.
func emptyRecycleBin(root string) error {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return err
	}
	const noConfirmation, noProgressUI, noSound = 1, 2, 4
	r, _, _ := pSHEmptyRecycleBin.Call(0, uintptr(unsafe.Pointer(p)), noConfirmation|noProgressUI|noSound)
	if r != 0 && r != 0x8000FFFF { // E_UNEXPECTED is what an already-empty bin reports
		return fmt.Errorf("Windows could not empty the Recycle Bin (code 0x%X)", r)
	}
	return nil
}

// shortName is the last part of a path in its old 8.3 form ("RUNNER~1" for "runneradmin"), or "" when it has none.
func shortName(path string) string {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil || path == "" {
		return ""
	}
	buf := make([]uint16, 520)
	n, err := syscall.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		return ""
	}
	return filepath.Base(syscall.UTF16ToString(buf[:n]))
}
