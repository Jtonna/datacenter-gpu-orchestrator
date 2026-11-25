//go:build !linux

package modelfiles

// mountReadOnlyOf: there is no mountinfo outside Linux.
func mountReadOnlyOf(string) (bool, string, bool) { return false, "", false }
