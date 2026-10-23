package engine

import (
	"errors"
	"testing"
)

// TestDirLock 验证同一目录不能被两个实例同时打开，Close 后可重新打开。
func TestDirLock(t *testing.T) {
	dir := t.TempDir()

	db1, err := Open(dir, &Options{})
	if err != nil {
		t.Fatalf("首次 Open 失败: %v", err)
	}
	if _, err := Open(dir, &Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("重复 Open 期望 ErrLocked，得到 %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	db2, err := Open(dir, &Options{})
	if err != nil {
		t.Fatalf("Close 后重新 Open 失败: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("第二次 Close 失败: %v", err)
	}
}
