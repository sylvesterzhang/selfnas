//go:build !windows && !linux && !darwin

package main

func listDrives() []DriveInfo { return nil }

func volumeSpaceOS(path string) (uint64, uint64) { return 0, 0 }
