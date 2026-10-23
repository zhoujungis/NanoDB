//go:build windows

package engine

import (
	"os"
	"syscall"
	"unsafe"
)

// LockFileEx 未在标准库 syscall 中导出，这里直接调用 kernel32，
// 避免为一把目录锁引入第三方依赖。
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

// lockDir 对锁文件加独占锁；锁由操作系统持有，进程退出时自动释放。
func lockDir(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(
		f.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r == 0 {
		return winCallError(err)
	}
	return nil
}

func unlockDir(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		return winCallError(err)
	}
	return nil
}

// winCallError 把 syscall 的 lastErr 转成 error；成功时 lastErr 可能为 nil。
func winCallError(err error) error {
	if err == nil || err == syscall.Errno(0) {
		return syscall.EINVAL
	}
	return err
}
