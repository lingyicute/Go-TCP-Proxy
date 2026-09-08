//go:build !unix && !windows

package tunnel

import "syscall"

func isConnClosedErrno(errno syscall.Errno) bool { return false }
