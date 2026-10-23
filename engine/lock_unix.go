//go:build !windows

package engine

import (
	"os"
	"syscall"
)

// lockDir 对锁文件加独占锁；锁由操作系统持有，进程退出时自动释放。
func lockDir(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockDir(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
