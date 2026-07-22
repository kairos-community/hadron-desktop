package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Disk is a candidate install target.
type Disk struct {
	Path   string
	SizeGB int
}

// pseudoPrefixes are /sys/block entries that are never valid install targets:
// loopback, optical, ramdisks, device-mapper and floppies. Same exclusion list
// as the shell installer this replaces.
var pseudoPrefixes = []string{"loop", "sr", "ram", "dm-", "fd"}

// ListDisks enumerates whole disks under sysBlock (normally /sys/block). The
// `size` file is in 512-byte sectors regardless of the device's logical block
// size, so GB is sectors/2/1024/1024.
//
// sysBlock is a parameter rather than a constant so tests can point it at a
// fixture tree instead of the host's real block devices.
func ListDisks(sysBlock string) ([]Disk, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return nil, err
	}
	var out []Disk
	for _, e := range entries {
		name := e.Name()
		if isPseudo(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(sysBlock, name, "size"))
		if err != nil {
			continue
		}
		sectors, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || sectors <= 0 {
			continue
		}
		out = append(out, Disk{
			Path:   "/dev/" + name,
			SizeGB: int(sectors / 2 / 1024 / 1024),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func isPseudo(name string) bool {
	for _, p := range pseudoPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
