package runningprogram

import (
	"fmt"
	"os"
	"reflect"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// These are the public Darwin proc_regionwithpathinfo fields from
// <sys/proc_info.h>. proc_info selects the VM region containing our own text,
// and its kernel vnode must match the opened file. os.Executable alone cannot
// distinguish a newly replaced file from the mapped executable.
type regionInfo struct {
	Protection, MaxProtection, Inheritance, Flags                                        uint32
	Offset                                                                               uint64
	Behavior, Wired, UserTag, Resident, Private, Swapped, Dirtied                        uint32
	References, ShadowDepth, ShareMode, PrivateResident, SharedResident, ObjectID, Depth uint32
	Address, Size                                                                        uint64
}

type vnodeStat struct {
	Device                                                                     uint32
	Mode, Links                                                                uint16
	Inode                                                                      uint64
	UID, GID                                                                   uint32
	Access, AccessNS, Modified, ModifiedNS, Changed, ChangedNS, Birth, BirthNS int64
	Size, Blocks                                                               int64
	BlockSize                                                                  int32
	Flags, Generation, RDevice                                                 uint32
	Spare                                                                      [2]int64
}

type regionPath struct {
	Region        regionInfo
	Stat          vnodeStat
	Type, Padding int32
	FSID          [2]int32
	Path          [1024]byte
}

//go:noinline
func imageAnchor() {}

func openExecuting() (*os.File, error) {
	address := reflect.ValueOf(imageAnchor).Pointer()
	var region regionPath
	// PROC_INFO_CALL_PIDINFO = 2; PROC_PIDREGIONPATHINFO = 8.
	n, _, errno := syscall.Syscall6(unix.SYS_PROC_INFO, 2, uintptr(os.Getpid()), 8, address, uintptr(unsafe.Pointer(&region)), unsafe.Sizeof(region))
	runtime.KeepAlive(&region)
	if errno != 0 {
		return nil, errno
	}
	if n != unsafe.Sizeof(region) || region.Region.Protection&4 == 0 || uint64(address) < region.Region.Address || uint64(address)-region.Region.Address >= region.Region.Size || region.Stat.Inode == 0 {
		return nil, fmt.Errorf("kernel did not identify executable text mapping")
	}
	path := unix.ByteSliceToString(region.Path[:])
	if path == "" {
		return nil, fmt.Errorf("mapped executable has no accessible path")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint32(stat.Dev) != region.Stat.Device || stat.Ino != region.Stat.Inode || stat.Size != region.Stat.Size {
		file.Close()
		return nil, fmt.Errorf("opened executable differs from kernel text vnode")
	}
	return file, nil
}

func processStart() (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil {
		return "", err
	}
	started := info.Proc.P_starttime
	if started.Sec <= 0 {
		return "", fmt.Errorf("kernel process start time unavailable")
	}
	return fmt.Sprintf("%d.%06d", started.Sec, started.Usec), nil
}
