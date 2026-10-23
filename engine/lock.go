package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// lockFileName 是数据目录下的独占锁文件，防止多个进程同时写同一个目录。
const lockFileName = "LOCK"

// acquireDirLock 打开并锁定目录锁文件；拿不到锁说明已有进程占用该目录。
func acquireDirLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("nanodb: open lock file: %w", err)
	}
	if err := lockDir(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrLocked, dir)
	}
	return f, nil
}

// releaseDirLock 释放锁并关闭锁文件；进程异常退出时由操作系统释放。
func releaseDirLock(f *os.File) {
	if f == nil {
		return
	}
	_ = unlockDir(f)
	_ = f.Close()
}
