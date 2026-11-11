//go:build windows

package tunnel

import (
	"errors"
	"syscall"
)

// Winsock 错误码。syscall 包只导出了其中一部分，其余按 WinError.h 补齐。
const (
	wsaeNotConn  syscall.Errno = 10057
	wsaeShutdown syscall.Errno = 10058
)

// isConnClosedError 报告 err 是否表示"对端已经断开"这一类常规情况。
func isConnClosedError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.WSAECONNRESET, syscall.WSAECONNABORTED, wsaeNotConn, wsaeShutdown,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ENOTCONN:
		return true
	}
	return false
}
