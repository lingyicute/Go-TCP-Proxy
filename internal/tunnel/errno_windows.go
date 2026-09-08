//go:build windows

package tunnel

import "syscall"

// Winsock 错误码。syscall 包只导出了其中一部分，其余按 WinError.h 补齐。
const (
	wsaeNotConn  syscall.Errno = 10057
	wsaeShutdown syscall.Errno = 10058
)

// isConnClosedErrno 报告 errno 是否表示"对端已经断开"这一类常规情况。
func isConnClosedErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.WSAECONNRESET, syscall.WSAECONNABORTED, wsaeNotConn, wsaeShutdown,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ENOTCONN:
		return true
	}
	return false
}
