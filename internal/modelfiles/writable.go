package modelfiles

import (
	"bufio"
	"io"
	"path"
	"strconv"
	"strings"
)

// Whether the models folder can be written, read without writing anything
// (no probe file, no mkdir), so a page can say "read-only" before an owner
// tries a delete or a restore:
//
//  1. Linux: the mount entry that holds the folder in /proc/self/mountinfo
//     (the longest mount point that is the folder or a parent of it; of two
//     mounts on one point the later, which is on top). Its options "ro"
//     (per mount, or the file system's own) answer false.
//  2. When that mount is rw, or mountinfo cannot be read, access(W_OK) on
//     the folder: a folder the process may not write answers false (EROFS
//     too), else true.
//  3. Where neither gives an answer (Windows), there is none: the caller
//     omits the field.

// Writable says whether dir can be written here; ok is false when it cannot
// be told.
func Writable(dir string) (writable, ok bool) {
	if ro, _, found := mountReadOnlyOf(dir); found && ro {
		return false, true
	}
	return accessWrite(dir)
}

// MountReadOnly reads mountinfo lines (proc(5)) and returns whether the
// mount that holds dir (an absolute, clean Linux path) is read-only, and
// its mount point. ok is false when no line holds it or dir is not absolute.
func MountReadOnly(r io.Reader, dir string) (ro bool, mountPoint string, ok bool) {
	if !strings.HasPrefix(dir, "/") {
		return false, "", false
	}
	dir = path.Clean(dir)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	best := -1
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if len(f) < 6 || sep < 0 {
			continue
		}
		mp := unescapeMount(f[4])
		if !(mp == "/" || dir == mp || strings.HasPrefix(dir, mp+"/")) {
			continue
		}
		if len(mp) < best {
			continue
		}
		best = len(mp)
		mountPoint = mp
		ro = hasOpt(f[5], "ro") || (sep+3 < len(f) && hasOpt(f[sep+3], "ro"))
		ok = true
	}
	return ro, mountPoint, ok
}

func hasOpt(opts, want string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// unescapeMount undoes mountinfo's octal escapes (\040 a space, \011 a tab,
// \012 a newline, \134 a backslash).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
