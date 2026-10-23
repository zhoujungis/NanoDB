package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newID 生成按时间单调递增的字符串 ID，便于按插入顺序排序。
func newID() string {
	var b [6]byte
	now := time.Now()
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时退化为纳秒低位，仍保持 25 位十六进制 ID。
		return fmt.Sprintf("%013x%012x", now.UnixMilli(), uint64(now.UnixNano())&0xffffffffffff)
	}
	return fmt.Sprintf("%013x%s", now.UnixMilli(), hex.EncodeToString(b[:]))
}
