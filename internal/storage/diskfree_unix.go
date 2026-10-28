//go:build linux || darwin || freebsd

package storage

import "syscall"

// diskFree returns the bytes free for unprivileged users on the filesystem of path
func diskFree(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
