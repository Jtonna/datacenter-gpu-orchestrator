package modelfiles

// accessWrite: Windows has no access(W_OK) that tells a read-only share or
// a missing permission without writing, so there is no answer here (the
// caller omits models_writable; the page behaves as without the field).
func accessWrite(string) (writable, ok bool) { return false, false }
