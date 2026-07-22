//go:build !windows

package sessions

func isRetryableAtomicReplaceError(error) bool {
	return false
}
