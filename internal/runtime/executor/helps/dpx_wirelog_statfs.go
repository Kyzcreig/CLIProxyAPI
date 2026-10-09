//go:build darwin || linux

package helps

import "syscall"

// dpxStatfsFree returns the bytes available to an unprivileged writer on the
// filesystem holding dir.
func dpxStatfsFree(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
