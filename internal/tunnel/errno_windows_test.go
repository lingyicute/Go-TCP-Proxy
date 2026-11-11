//go:build windows

package tunnel

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// Windows 上对端复位有几种报法，都不值得记进日志；其余的系统错误仍然要记。
func TestIsBenignError_WindowsResetCodes(t *testing.T) {
	wrap := func(errno syscall.Errno) error {
		return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", errno)}
	}
	for _, errno := range []syscall.Errno{
		syscall.WSAECONNRESET, syscall.WSAECONNABORTED, syscall.ERROR_NETNAME_DELETED, wsaeNotConn, wsaeShutdown,
	} {
		if !isBenignError(wrap(errno)) {
			t.Errorf("%v 应被视为常规错误", errno)
		}
	}
	if isBenignError(wrap(syscall.ETIMEDOUT)) {
		t.Errorf("ETIMEDOUT 不应被视为常规错误")
	}
}
