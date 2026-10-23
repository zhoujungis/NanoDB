package engine

import (
	"errors"
	"testing"
)

func TestBatchAtomicAndPersistence(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	results, err := db.Batch([]BatchOp{
		{Op: "insert", Coll: "counters", Doc: map[string]any{"_id": "a", "v": 1}},
		{Op: "update", Coll: "counters", ID: "a", Patch: map[string]any{"$inc": map[string]any{"v": 2}}},
		{Op: "insert", Coll: "counters", Doc: map[string]any{"_id": "b", "v": 5}},
		{Op: "delete", Coll: "counters", ID: "missing"},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(results) != 4 || results[3].Deleted {
		t.Fatalf("批量结果错误: %+v", results)
	}
	a, ok := db.Get("counters", "a")
	if !ok || a["v"].(float64) != 3 {
		t.Fatalf("批量更新未生效: %v", a)
	}
	if n, _ := db.Count("counters", nil); n != 2 {
		t.Fatalf("批量写入后文档数 = %d, want 2", n)
	}

	// 第二步失败时，第一步插入也必须回滚。
	_, err = db.Batch([]BatchOp{
		{Op: "insert", Coll: "counters", Doc: map[string]any{"_id": "c", "v": 9}},
		{Op: "update", Coll: "counters", ID: "no-such", Patch: map[string]any{"$set": map[string]any{"v": 1}}},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败批量 = %v, want ErrNotFound", err)
	}
	if _, ok := db.Get("counters", "c"); ok {
		t.Fatal("失败批量中的插入不应生效")
	}
	if n, _ := db.Count("counters", nil); n != 2 {
		t.Fatalf("失败批量后文档数 = %d, want 2", n)
	}

	// 批内重复 _id 也应整体回滚。
	_, err = db.Batch([]BatchOp{
		{Op: "insert", Coll: "counters", Doc: map[string]any{"_id": "d"}},
		{Op: "insert", Coll: "counters", Doc: map[string]any{"_id": "d"}},
	})
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("批内重复 _id = %v, want ErrDuplicateID", err)
	}
	if _, ok := db.Get("counters", "d"); ok {
		t.Fatal("批内重复 _id 时不应留下任何文档")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openTestDB(t, dir)
	if n, _ := reopened.Count("counters", nil); n != 2 {
		t.Fatalf("重启后文档数 = %d, want 2", n)
	}
	a, ok = reopened.Get("counters", "a")
	if !ok || a["v"].(float64) != 3 {
		t.Fatalf("重启后批量更新丢失: %v", a)
	}
}

func TestBatchUniqueIndexRollback(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	if err := db.CreateIndex("users", "email", true); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if _, err := db.Insert("users", map[string]any{"_id": "exists", "email": "x@example.com"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	_, err := db.Batch([]BatchOp{
		{Op: "insert", Coll: "users", Doc: map[string]any{"_id": "ok", "email": "y@example.com"}},
		{Op: "insert", Coll: "users", Doc: map[string]any{"_id": "bad", "email": "x@example.com"}},
	})
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("批量唯一冲突 = %v, want ErrDuplicateKey", err)
	}
	if _, ok := db.Get("users", "ok"); ok {
		t.Fatal("唯一冲突时整批插入都应回滚")
	}
	if n, _ := db.Count("users", nil); n != 1 {
		t.Fatalf("唯一冲突后文档数 = %d, want 1", n)
	}
}

func TestBatchWithSmallCompactionThreshold(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, &Options{Sync: false, CompactThreshold: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for round := 0; round < 20; round++ {
		ops := []BatchOp{
			{Op: "insert", Coll: "logs", Doc: map[string]any{"round": round, "kind": "a"}},
			{Op: "insert", Coll: "logs", Doc: map[string]any{"round": round, "kind": "b"}},
		}
		if _, err := db.Batch(ops); err != nil {
			t.Fatalf("Batch: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openTestDB(t, dir)
	if n, _ := reopened.Count("logs", nil); n != 40 {
		t.Fatalf("压缩后重启文档数 = %d, want 40", n)
	}
}
