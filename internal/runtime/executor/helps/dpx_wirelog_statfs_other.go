//go:build !darwin && !linux

package helps

// dpxStatfsFree is unknown off darwin/linux; the floor then does not block capture.
func dpxStatfsFree(string) (uint64, bool) { return 0, false }
