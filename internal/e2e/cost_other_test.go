//go:build !windows

package e2e

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// procCPU is the process's CPU time so far (utime and stime of
// /proc/<pid>/stat, at 100 ticks a second), 0 when it cannot be read.
func procCPU(pid int) time.Duration {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+2:])
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.ParseInt(f[11], 10, 64)
	k, _ := strconv.ParseInt(f[12], 10, 64)
	return time.Duration(u+k) * 10 * time.Millisecond
}
