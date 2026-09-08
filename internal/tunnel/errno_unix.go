//go:build unix

package tunnel

import "syscall"

// isConnClosedErrno 报告 errno 是否表示"对端已经断开"这一类常规情况。
func isConnClosedErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ENOTCONN:
		return true
	}
	return false
}
