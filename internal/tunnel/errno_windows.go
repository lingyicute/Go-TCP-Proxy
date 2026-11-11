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
//
// ERROR_NETNAME_DELETED（"指定的网络名不再可用"）是 IOCP 上连接被对端复位时的另一种报法：
// 标准库自己也把它与 WSAECONNRESET 同等对待（见 internal/poll 的 accept 实现："Sometimes we
// see WSAECONNRESET and ERROR_NETNAME_DELETED"），漏掉它，正常的对端复位就会被当成错误打进日志。
func isConnClosedError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.WSAECONNRESET, syscall.WSAECONNABORTED, wsaeNotConn, wsaeShutdown, syscall.ERROR_NETNAME_DELETED,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ENOTCONN:
		return true
	}
	return false
}
