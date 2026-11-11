//go:build unix

package tunnel

import (
	"errors"
	"syscall"
)

// isConnClosedError 报告 err 是否表示"对端已经断开"这一类常规情况。
//
// EPROTOTYPE 是 macOS 的一个老毛病：连接正在被对端复位（RST 已到达、内核正在拆连接）时，
// 恰好在进行的 write(2) 会返回 EPROTOTYPE（"protocol wrong type for socket"）而不是
// EPIPE/ECONNRESET。对 TCP socket 来说它不可能有别的来源，所以同样视作"对端已断开"。
func isConnClosedError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ENOTCONN, syscall.ESHUTDOWN, syscall.EPROTOTYPE:
		return true
	}
	return false
}
