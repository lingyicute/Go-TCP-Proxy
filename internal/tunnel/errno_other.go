//go:build !unix && !windows

package tunnel

// isConnClosedError 在其余平台上不识别任何系统错误码。这里不能出现 syscall.Errno：
// Plan 9 的系统调用以字符串报错（syscall.ErrorString），没有这个类型，引用它整个包都编译不过。
func isConnClosedError(error) bool { return false }
