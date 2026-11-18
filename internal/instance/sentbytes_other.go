//go:build !linux

package instance

func socketBytesAcked(pid int, peers []string) (SentCount, error) {
	return SentCount{}, ErrSentBytesUnsupported
}
