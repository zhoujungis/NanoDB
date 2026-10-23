package engine

import (
	"errors"
	"testing"
)

func TestTransactionCommitAndPersistence(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	alice, err := tx.Insert("users", map[string]any{"name": "Alice", "age": 30})
	if err != nil {
		t.Fatalf("tx.Insert: %v", err)
	}
	id, _ := alice["_id"].(string)
	if _, err := tx.Insert("orders", map[string]any{"user": id, "amount": 99}); err != nil {
		t.Fatalf("tx.Insert orders: %v", err)
	}
	if _, err := tx.Update("users", id, map[string]any{"$inc": map[string]any{"age": 1}}); err != nil {
		t.Fatalf("tx.Update: %v", err)
	}

	// 事务内应能看到自己尚未提交的写入。
	if got, ok := tx.Get("users", id); !ok || got["age"].(float64) != 31 {
		t.Fatalf("事务内读取 = %v, %v", got, ok)
	}
	if _, ok := db.Get("users", id); ok {
		t.Fatal("提交前其他连接不应看到事务内写入")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got, ok := db.Get("users", id); !ok || got["age"].(float64) != 31 {
		t.Fatalf("提交后读取 = %v, %v", got, ok)
	}
	if n, err := db.Count("orders", nil); err != nil || n != 1 {
		t.Fatalf("orders count = %d, err=%v", n, err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, &Options{Sync: false})
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get("users", id); !ok || got["age"].(float64) != 31 {
		t.Fatalf("重启后读取 = %v, %v", got, ok)
	}
	if n, err := reopened.Count("orders", nil); err != nil || n != 1 {
		t.Fatalf("重启后 orders count = %d, err=%v", n, err)
	}
}

func TestTransactionRollback(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	doc, err := db.Insert("users", map[string]any{"name": "Alice", "age": 30})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id := doc["_id"].(string)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Update("users", id, map[string]any{"$set": map[string]any{"age": 99}}); err != nil {
		t.Fatalf("tx.Update: %v", err)
	}
	if _, err := tx.Insert("users", map[string]any{"name": "Bob"}); err != nil {
		t.Fatalf("tx.Insert: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	got, ok := db.Get("users", id)
	if !ok || got["age"] != 30 {
		t.Fatalf("回滚后文档 = %v, %v", got, ok)
	}
	if n, err := db.Count("users", nil); err != nil || n != 1 {
		t.Fatalf("回滚后 count = %d, err=%v", n, err)
	}

	if _, err := tx.Find("users", Query{}); !errors.Is(err, ErrTxDone) {
		t.Fatalf("回滚后使用事务期望 ErrTxDone，得到 %v", err)
	}
	if err := tx.Rollback(); !errors.Is(err, ErrTxDone) {
		t.Fatalf("重复回滚期望 ErrTxDone，得到 %v", err)
	}
}

func TestTransactionConflict(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	doc, err := db.Insert("users", map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id := doc["_id"].(string)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, ok := tx.Get("users", id); !ok {
		t.Fatal("事务内读取失败")
	}
	if _, err := tx.Insert("users", map[string]any{"name": "Bob"}); err != nil {
		t.Fatalf("tx.Insert: %v", err)
	}

	// 事务进行期间另一个写入修改了 users，提交必须失败。
	if _, err := db.Insert("users", map[string]any{"name": "Cara"}); err != nil {
		t.Fatalf("并发 Insert: %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit 期望 ErrConflict，得到 %v", err)
	}

	if n, err := db.Count("users", nil); err != nil || n != 2 {
		t.Fatalf("冲突回滚后 count = %d, err=%v", n, err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrTxDone) {
		t.Fatalf("冲突后事务应结束，得到 %v", err)
	}
}

func TestTransactionReadOnlyCommit(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	if _, err := db.Insert("users", map[string]any{"name": "Alice", "age": 30}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	n, err := tx.Count("users", map[string]any{"age": map[string]any{"$gte": 18}})
	if err != nil || n != 1 {
		t.Fatalf("tx.Count = %d, err=%v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("只读事务 Commit: %v", err)
	}

	// 只读事务同样校验快照一致性：读取后集合被修改，提交返回冲突。
	tx2, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx2.Find("users", Query{}); err != nil {
		t.Fatalf("tx.Find: %v", err)
	}
	if _, err := db.Insert("users", map[string]any{"name": "Bob"}); err != nil {
		t.Fatalf("并发 Insert: %v", err)
	}
	if err := tx2.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("只读事务冲突期望 ErrConflict，得到 %v", err)
	}
	if n, err := db.Count("users", nil); err != nil || n != 2 {
		t.Fatalf("count = %d, err=%v", n, err)
	}
}

func TestTransactionUniqueIndex(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	if err := db.CreateIndex("users", "email", true); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if _, err := db.Insert("users", map[string]any{"email": "a@example.com"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Insert("users", map[string]any{"email": "b@example.com"}); err != nil {
		t.Fatalf("tx.Insert: %v", err)
	}
	if _, err := tx.Insert("users", map[string]any{"email": "a@example.com"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("唯一索引冲突期望 ErrDuplicateKey，得到 %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if n, err := db.Count("users", nil); err != nil || n != 1 {
		t.Fatalf("回滚后 count = %d, err=%v", n, err)
	}
}

func TestTransactionBatchAtomic(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, err = tx.Batch([]BatchOp{
		{Op: "insert", Coll: "users", ID: "u1", Doc: map[string]any{"_id": "u1", "name": "Alice"}},
		{Op: "update", Coll: "users", ID: "missing", Patch: map[string]any{"$set": map[string]any{"x": 1}}},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败批次期望 ErrNotFound，得到 %v", err)
	}
	if _, ok := tx.Get("users", "u1"); ok {
		t.Fatal("失败的批次不应留下部分写入")
	}

	results, err := tx.Batch([]BatchOp{
		{Op: "insert", Coll: "users", ID: "u1", Doc: map[string]any{"_id": "u1", "name": "Alice"}},
		{Op: "insert", Coll: "logs", Doc: map[string]any{"msg": "created"}},
	})
	if err != nil {
		t.Fatalf("tx.Batch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	if _, ok := tx.Get("users", "u1"); !ok {
		t.Fatal("事务内未看到批量写入")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, ok := db.Get("users", "u1"); !ok {
		t.Fatal("提交后文档丢失")
	}
	if n, err := db.Count("logs", nil); err != nil || n != 1 {
		t.Fatalf("logs count = %d, err=%v", n, err)
	}
}
