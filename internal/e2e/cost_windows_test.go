package e2e

import (
	"time"

	"golang.org/x/sys/windows"
)

// procCPU is the process's CPU time so far (user and kernel), 0 when it
// cannot be read.
func procCPU(pid int) time.Duration {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0
	}
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration(int64(f.HighDateTime)<<32|int64(f.LowDateTime)) * 100
	}
	return ft(k) + ft(u)
}
