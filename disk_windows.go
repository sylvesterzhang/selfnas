//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDrives      = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW         = kernel32.NewProc("GetDriveTypeW")
	procGetVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
	procGetDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
)

func listDrives() []DriveInfo {
	mask, _, _ := procGetLogicalDrives.Call()
	var out []DriveInfo
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		path := letter + ":\\"
		p, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		dt, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(p)))
		// 1=无根目录 5=光驱，均跳过
		if dt == 1 || dt == 5 {
			continue
		}
		info := DriveInfo{Letter: letter, Path: path}
		switch dt {
		case 2:
			info.Kind = "removable"
		case 3:
			info.Kind = "fixed"
		case 4:
			info.Kind = "network"
		case 6:
			info.Kind = "ramdisk"
		default:
			info.Kind = "unknown"
		}

		var volName [261]uint16
		var fsName [261]uint16
		var serial, maxComp, flags uint32
		r, _, _ := procGetVolumeInformationW.Call(
			uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&volName[0])), uintptr(len(volName)),
			uintptr(unsafe.Pointer(&serial)),
			uintptr(unsafe.Pointer(&maxComp)),
			uintptr(unsafe.Pointer(&flags)),
			uintptr(unsafe.Pointer(&fsName[0])), uintptr(len(fsName)),
		)
		if r != 0 {
			info.Ready = true
			info.Label = syscall.UTF16ToString(volName[:])
		}

		if total, free, ok := diskSpace(path); ok {
			info.TotalBytes = total
			info.FreeBytes = free
			info.Ready = true
		}
		out = append(out, info)
	}
	return out
}

func diskSpace(path string) (uint64, uint64, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var freeAvail, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, 0, false
	}
	return total, freeAvail, true
}

// volumeSpaceOS 取某个路径所在卷的总容量与可用空间
func volumeSpaceOS(path string) (uint64, uint64) {
	if len(path) >= 2 && path[1] == ':' {
		root := fmt.Sprintf("%c:\\", path[0])
		if total, free, ok := diskSpace(root); ok {
			return total, free
		}
	}
	return 0, 0
}
