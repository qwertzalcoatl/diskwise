package main

import (
	"os/exec"
	"strings"
	"syscall"
)

type VolumeInfo struct {
	Mount     string   `json:"mount"`
	Capacity  int64    `json:"capacity"`  // APFS container size
	Used      int64    `json:"used"`      // used by this volume
	Available int64    `json:"available"` // free for this volume
	Snapshots []string `json:"snapshots,omitempty"`
}

// Others is space used by sibling APFS volumes (System, VM, Preboot, ...)
// plus anything reserved; it is not visible to a scan of this volume.
func (v VolumeInfo) Others() int64 {
	o := v.Capacity - v.Used - v.Available
	if o < 0 {
		return 0
	}
	return o
}

func volumeInfo(path string) VolumeInfo {
	var st syscall.Statfs_t
	v := VolumeInfo{}
	if err := syscall.Statfs(path, &st); err == nil {
		bs := int64(st.Bsize)
		v.Mount = strings.TrimRight(string(int8sToBytes(st.Mntonname[:])), "\x00")
		v.Capacity = int64(st.Blocks) * bs
		v.Used = (int64(st.Blocks) - int64(st.Bfree)) * bs
		v.Available = int64(st.Bavail) * bs
	}
	if out, err := exec.Command("tmutil", "listlocalsnapshots", "/").Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "com.apple.") {
				v.Snapshots = append(v.Snapshots, l)
			}
		}
	}
	return v
}

func int8sToBytes(a []int8) []byte {
	b := make([]byte, 0, len(a))
	for _, c := range a {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return b
}

func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
