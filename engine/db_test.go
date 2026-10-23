package engine

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTestDB(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(dir, &Options{Sync: false})
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCRUDAndPersistence(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	input := map[string]any{"name": "Alice", "age": 30, "tags": []any{"admin"}}
	doc, err := db.Insert("users", input)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, ok := input["_id"]; ok {
		t.Fatal("Insert 不应修改调用方传入的 map")
	}
	id, _ := doc["_id"].(string)
	if id == "" {
		t.Fatal("自动生成的 _id 为空")
	}

	if _, err := db.Insert("users", map[string]any{"_id": id}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("重复 _id 期望 ErrDuplicateID，得到 %v", err)
	}

	updated, err := db.Update("users", id, map[string]any{"$inc": map[string]any{"age": 5}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if age, _ := updated["age"].(float64); age != 35 {
		t.Fatalf("age = %v, want 35", updated["age"])
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, &Options{Sync: false})
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer reopened.Close()

	got, ok := reopened.Get("users", id)
	if !ok {
		t.Fatal("重启后文档丢失")
	}
	if got["name"] != "Alice" {
		t.Fatalf("name = %v", got["name"])
	}
	if age, _ := got["age"].(float64); age != 35 {
		t.Fatalf("重启后 age = %v, want 35", got["age"])
	}
}

func TestInsertManyAndUpdateMany(t *testing.T) {
	db := openTestDB(t, t.TempDir())

	docs, err := db.InsertMany("users", []map[string]any{
		{"_id": "a", "age": 10},
		{"_id": "b", "age": 20},
		{"_id": "c", "age": 30},
	})
	if err != nil || len(docs) != 3 {
		t.Fatalf("InsertMany = %d docs, err=%v", len(docs), err)
	}

	partial, err := db.InsertMany("users", []map[string]any{{"_id": "d"}, {"_id": "a"}})
	if !errors.Is(err, ErrDuplicateID) || len(partial) != 1 {
		t.Fatalf("部分失败应返回已插入的 1 条，得到 %d, err=%v", len(partial), err)
	}
	if _, ok := db.Get("users", "d"); !ok {
		t.Fatal("失败前已插入的文档应保留")
	}

	n, err := db.UpdateMany("users", map[string]any{"age": map[string]any{"$gte": 20}}, map[string]any{"$inc": map[string]any{"age": 1}})
	if err != nil || n != 2 {
		t.Fatalf("UpdateMany = %d, err=%v", n, err)
	}
	if got, _ := db.Get("users", "b"); got["age"] != float64(21) {
		t.Fatalf("b.age = %v, want 21", got["age"])
	}

	if _, err := db.UpdateMany("users", nil, map[string]any{"$inc": map[string]any{"age": "x"}}); err == nil {
		t.Fatal("非法更新表达式应返回错误")
	}
	if n, err := db.UpdateMany("missing", nil, map[string]any{"$set": map[string]any{"a": 1}}); err != nil || n != 0 {
		t.Fatalf("不存在的集合应返回 0，得到 %d, err=%v", n, err)
	}
}

func TestMutationAfterClose(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.InsertMany("users", []map[string]any{{"a": 1}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("InsertMany 期望 ErrClosed，得到 %v", err)
	}
	if _, err := db.UpdateMany("users", nil, map[string]any{"$set": map[string]any{"a": 1}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("UpdateMany 期望 ErrClosed，得到 %v", err)
	}
}

func TestTxMutationAPIs(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	if _, err := db.Insert("users", map[string]any{"_id": "a", "age": 1}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.InsertMany("users", []map[string]any{{"_id": "b", "age": 2}, {"_id": "c", "age": 3}}); err != nil {
		t.Fatalf("Tx.InsertMany: %v", err)
	}
	replaced, err := tx.Replace("users", "a", map[string]any{"_id": "zzz", "name": "A"})
	if err != nil {
		t.Fatalf("Tx.Replace: %v", err)
	}
	if replaced["_id"] != "a" {
		t.Fatalf("Replace 不应改变 _id，得到 %v", replaced["_id"])
	}
	if _, err := tx.Replace("users", "missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("替换不存在的文档期望 ErrNotFound，得到 %v", err)
	}
	if ok, err := tx.Delete("users", "b"); err != nil || !ok {
		t.Fatalf("Tx.Delete = %v, err=%v", ok, err)
	}
	if ok, _ := tx.Delete("users", "b"); ok {
		t.Fatal("重复删除应返回 false")
	}
	if n, err := tx.UpdateMany("users", map[string]any{"age": map[string]any{"$gte": 1}}, map[string]any{"$inc": map[string]any{"age": 1}}); err != nil || n != 1 {
		t.Fatalf("Tx.UpdateMany = %d, err=%v", n, err)
	}
	if n, err := tx.DeleteMany("users", map[string]any{"age": map[string]any{"$gte": 4}}); err != nil || n != 1 {
		t.Fatalf("Tx.DeleteMany = %d, err=%v", n, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got, ok := db.Get("users", "a"); !ok || got["age"] != 1 {
		t.Fatalf("回滚后 a 应恢复，得到 %v, ok=%v", got, ok)
	}
	if _, ok := db.Get("users", "b"); ok {
		t.Fatal("回滚后事务内插入的 b 不应存在")
	}
	if _, ok := db.Get("users", "c"); ok {
		t.Fatal("回滚后事务内插入的 c 不应存在")
	}

	done, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := done.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := done.InsertMany("users", []map[string]any{{"_id": "x"}}); !errors.Is(err, ErrTxDone) {
		t.Fatalf("已结束事务期望 ErrTxDone，得到 %v", err)
	}
	if _, err := done.Replace("users", "a", nil); !errors.Is(err, ErrTxDone) {
		t.Fatalf("已结束事务期望 ErrTxDone，得到 %v", err)
	}
	if _, err := done.Delete("users", "a"); !errors.Is(err, ErrTxDone) {
		t.Fatalf("已结束事务期望 ErrTxDone，得到 %v", err)
	}
	if _, err := done.UpdateMany("users", nil, map[string]any{"$set": map[string]any{"a": 1}}); !errors.Is(err, ErrTxDone) {
		t.Fatalf("已结束事务期望 ErrTxDone，得到 %v", err)
	}
	if _, err := done.DeleteMany("users", nil); !errors.Is(err, ErrTxDone) {
		t.Fatalf("已结束事务期望 ErrTxDone，得到 %v", err)
	}
}

func TestFilterOrWithIndex(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	for _, age := range []int{10, 20, 30} {
		if _, err := db.Insert("users", map[string]any{"age": age}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if err := db.CreateIndex("users", "age", false); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	docs, err := db.Find("users", Query{Filter: map[string]any{
		"$or": []any{
			map[string]any{"age": 10},
			map[string]any{"age": 30},
		},
	}})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("$or + 索引应命中 2 条，得到 %d", len(docs))
	}
}

func TestReopenReplaysAllWalOps(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	if _, err := db.Insert("users", map[string]any{"_id": "a", "age": 1}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := db.Insert("users", map[string]any{"_id": "b", "age": 2}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := db.CreateIndex("users", "age", false); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if _, err := db.Update("users", "a", map[string]any{"$inc": map[string]any{"age": 10}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if ok, err := db.Delete("users", "b"); err != nil || !ok {
		t.Fatalf("Delete = %v, err=%v", ok, err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Insert("logs", map[string]any{"_id": "l1", "level": "info"}); err != nil {
		t.Fatalf("Tx.Insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, err := db.Insert("temp", map[string]any{"_id": "t1"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := db.DropCollection("temp"); err != nil {
		t.Fatalf("DropCollection: %v", err)
	}
	if err := db.CreateIndex("logs", "level", false); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := db.DropIndex("logs", "level"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, &Options{Sync: false})
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer reopened.Close()

	got, ok := reopened.Get("users", "a")
	if !ok || got["age"] != float64(11) {
		t.Fatalf("重开后 a.age = %v, ok=%v", got["age"], ok)
	}
	if _, ok := reopened.Get("users", "b"); ok {
		t.Fatal("删除的文档在重开后不应存在")
	}
	if _, ok := reopened.Get("temp", "t1"); ok {
		t.Fatal("drop 的集合在重开后不应存在")
	}
	if _, ok := reopened.Get("logs", "l1"); !ok {
		t.Fatal("事务提交的文档在重开后丢失")
	}
	indexes := reopened.ListIndexes("users")
	if len(indexes) != 1 || indexes[0].Field != "age" {
		t.Fatalf("users 索引未恢复: %#v", indexes)
	}
	if index := reopened.ListIndexes("logs"); len(index) != 0 {
		t.Fatalf("drop 的索引不应恢复: %#v", index)
	}
	byAge, err := reopened.Find("users", Query{Filter: map[string]any{"age": map[string]any{"$gte": 10}}})
	if err != nil || len(byAge) != 1 {
		t.Fatalf("重开后索引查询 = %d, err=%v", len(byAge), err)
	}
}

func TestUpdateOperatorErrors(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	if _, err := db.Insert("items", map[string]any{
		"_id": "i1", "n": 1, "s": "x", "tags": []any{1, 2, 3, 4},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	invalid := []map[string]any{
		{"$inc": map[string]any{"n": "1"}},
		{"$inc": map[string]any{"s": 1}},
		{"$push": map[string]any{"s": "x"}},
		{"$addToSet": map[string]any{"s": "x"}},
		{"$pull": map[string]any{"s": "x"}},
		{"$nope": map[string]any{"n": 1}},
	}
	for _, patch := range invalid {
		if _, err := db.Update("items", "i1", patch); !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("Update %v 期望 ErrInvalidUpdate，得到 %v", patch, err)
		}
	}
	if got, _ := db.Get("items", "i1"); got["n"] != 1 || got["s"] != "x" {
		t.Fatalf("失败的更新不应改动文档: %v", got)
	}

	if _, err := db.Update("items", "i1", map[string]any{"$unset": map[string]any{"missing": ""}}); err != nil {
		t.Fatalf("$unset 不存在的字段不应报错: %v", err)
	}
	updated, err := db.Update("items", "i1", map[string]any{"$pull": map[string]any{"tags": map[string]any{"$gte": 3}}})
	if err != nil {
		t.Fatalf("$pull 条件: %v", err)
	}
	tags, _ := updated["tags"].([]any)
	if len(tags) != 2 || tags[0] != 1 || tags[1] != 2 {
		t.Fatalf("$pull 条件结果 = %v", tags)
	}
}

func TestFindSortLimitProjection(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	docs := []map[string]any{
		{"name": "Alice", "age": 30, "active": true, "profile": map[string]any{"city": "Shanghai"}},
		{"name": "Bob", "age": 18, "active": false, "profile": map[string]any{"city": "Beijing"}},
		{"name": "Cara", "age": 42, "active": true, "profile": map[string]any{"city": "Shanghai"}},
		{"name": "Dan", "age": 25, "active": true},
	}
	for _, doc := range docs {
		if _, err := db.Insert("users", doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	got, err := db.Find("users", Query{Filter: map[string]any{"age": map[string]any{"$gte": 25}}})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("条件查询返回 %d 条，期望 3", len(got))
	}

	sorted, err := db.Find("users", Query{Sort: []SortField{{Field: "age", Desc: true}}})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if sorted[0]["name"] != "Cara" || sorted[3]["name"] != "Bob" {
		t.Fatalf("降序排序结果错误: %v", sorted)
	}

	page, err := db.Find("users", Query{
		Sort:  []SortField{{Field: "age"}},
		Skip:  1,
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(page) != 2 || page[0]["name"] != "Dan" || page[1]["name"] != "Alice" {
		t.Fatalf("分页结果错误: %v", page)
	}

	projected, err := db.Find("users", Query{
		Filter: map[string]any{"name": "Alice"},
		Fields: []string{"name", "profile.city"},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(projected) != 1 {
		t.Fatalf("投影返回 %d 条", len(projected))
	}
	only := projected[0]
	if _, ok := only["age"]; ok {
		t.Fatalf("投影不应包含 age: %v", only)
	}
	if only["name"] != "Alice" {
		t.Fatalf("投影缺少 name: %v", only)
	}
	profile, _ := only["profile"].(map[string]any)
	if profile["city"] != "Shanghai" {
		t.Fatalf("投影缺少 profile.city: %v", only)
	}

	n, err := db.Count("users", map[string]any{"active": true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Fatalf("Count = %d, want 3", n)
	}
}

func TestUpdateOperators(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	doc, err := db.Insert("items", map[string]any{
		"name":  "box",
		"count": 1,
		"tags":  []any{"a"},
		"meta":  map[string]any{"city": "Shanghai", "zip": "200000"},
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id := doc["_id"].(string)

	out, err := db.Update("items", id, map[string]any{
		"$set":      map[string]any{"meta.city": "Beijing"},
		"$unset":    map[string]any{"meta.zip": ""},
		"$inc":      map[string]any{"count": 2},
		"$push":     map[string]any{"tags": "b"},
		"$addToSet": map[string]any{"tags": "a"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out["count"].(float64) != 3 {
		t.Fatalf("$inc 结果 = %v", out["count"])
	}
	meta := out["meta"].(map[string]any)
	if meta["city"] != "Beijing" {
		t.Fatalf("$set 嵌套字段失败: %v", meta)
	}
	if _, ok := meta["zip"]; ok {
		t.Fatalf("$unset 未生效: %v", meta)
	}
	tags := out["tags"].([]any)
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Fatalf("$push/$addToSet 结果 = %v", tags)
	}

	out, err = db.Update("items", id, map[string]any{"$pull": map[string]any{"tags": "a"}})
	if err != nil {
		t.Fatalf("Update $pull: %v", err)
	}
	if tags := out["tags"].([]any); len(tags) != 1 || tags[0] != "b" {
		t.Fatalf("$pull 结果 = %v", tags)
	}

	if _, err := db.Update("items", id, map[string]any{"name": "oops"}); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("无操作符的更新应报错，得到 %v", err)
	}
	if _, err := db.Update("items", id, map[string]any{"$set": map[string]any{"_id": "x"}}); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("修改 _id 应报错，得到 %v", err)
	}
	if _, err := db.Update("items", id, map[string]any{"$inc": map[string]any{"name": 1}}); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("对字符串 $inc 应报错，得到 %v", err)
	}
	if _, err := db.Update("items", "missing", map[string]any{"$set": map[string]any{"a": 1}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("更新不存在文档应报 ErrNotFound，得到 %v", err)
	}
}

func TestDeleteManyAndDrop(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	for _, age := range []float64{10, 20, 30, 40} {
		if _, err := db.Insert("users", map[string]any{"age": age}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	n, err := db.DeleteMany("users", map[string]any{"age": map[string]any{"$lt": 20}})
	if err != nil || n != 1 {
		t.Fatalf("DeleteMany = %d, %v", n, err)
	}
	n, err = db.DeleteMany("users", map[string]any{"age": 30})
	if err != nil || n != 1 {
		t.Fatalf("DeleteMany = %d, %v", n, err)
	}
	if count, _ := db.Count("users", nil); count != 2 {
		t.Fatalf("剩余 %d 条，期望 2", count)
	}

	if err := db.DropCollection("users"); err != nil {
		t.Fatalf("DropCollection: %v", err)
	}
	if infos := db.Collections(); len(infos) != 0 {
		t.Fatalf("drop 后仍有集合: %v", infos)
	}
}

func TestCompactionAndRecovery(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, &Options{Sync: false, CompactThreshold: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 60; i++ {
		if _, err := db.Insert("logs", map[string]any{"i": i, "msg": "hello nanodb"}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotName)); err != nil {
		t.Fatalf("自动压缩后应存在快照文件: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openTestDB(t, dir)
	if count, _ := reopened.Count("logs", nil); count != 60 {
		t.Fatalf("恢复后 %d 条，期望 60", count)
	}
	if err := reopened.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, walName))
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("压缩后 WAL 大小 = %d，期望 0", info.Size())
	}
}

func TestGoTypedContainersNormalized(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	tags := []string{"go", "db"}
	profile := map[string]string{"city": "Shanghai"}
	if _, err := db.Insert("users", map[string]any{"_id": "u1", "tags": tags, "profile": profile}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// 写入后修改调用方容器，不应影响库内数据。
	tags[0] = "rust"
	profile["city"] = "Beijing"

	got, ok := db.Get("users", "u1")
	if !ok {
		t.Fatal("文档丢失")
	}
	gotTags, ok := got["tags"].([]any)
	if !ok || len(gotTags) != 2 || gotTags[0] != "go" || gotTags[1] != "db" {
		t.Fatalf("tags = %#v, want []any{\"go\",\"db\"}", got["tags"])
	}
	gotProfile, ok := got["profile"].(map[string]any)
	if !ok || gotProfile["city"] != "Shanghai" {
		t.Fatalf("profile = %#v, want map city=Shanghai", got["profile"])
	}

	// 归一化后的数组可以参与 multikey 索引与查询。
	if err := db.CreateIndex("users", "tags", false); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	docs, err := db.Find("users", Query{Filter: map[string]any{"tags": "go"}})
	if err != nil || len(docs) != 1 {
		t.Fatalf("multikey 查询 = %v, %v, want 1", docs, err)
	}
	// Go 调用方传入的 typed slice 作为过滤值也应被当作 JSON 数组处理。
	docs, err = db.Find("users", Query{Filter: map[string]any{"tags": map[string]any{"$in": []string{"db"}}}})
	if err != nil || len(docs) != 1 {
		t.Fatalf("$in typed slice 查询 = %v, %v, want 1", docs, err)
	}

	// $set 写入的 Go 容器同样会被归一化，支持点号路径查询。
	if _, err := db.Update("users", "u1", map[string]any{"$set": map[string]any{"meta": map[string]string{"lang": "zh"}}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	docs, err = db.Find("users", Query{Filter: map[string]any{"meta.lang": "zh"}})
	if err != nil || len(docs) != 1 {
		t.Fatalf("点号路径查询 = %v, %v, want 1", docs, err)
	}
}

func TestReopenSkipsWALCoveredBySnapshot(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	if err := db.CreateIndex("users", "email", true); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if _, err := db.Insert("users", map[string]any{"_id": "a", "email": "x@example.com"}); err != nil {
		t.Fatalf("Insert a: %v", err)
	}
	if _, err := db.Insert("users", map[string]any{"_id": "b", "email": "y@example.com"}); err != nil {
		t.Fatalf("Insert b: %v", err)
	}
	if _, err := db.Update("users", "a", map[string]any{"$set": map[string]any{"email": "z@example.com"}}); err != nil {
		t.Fatalf("Update a: %v", err)
	}
	if _, err := db.Update("users", "b", map[string]any{"$set": map[string]any{"email": "x@example.com"}}); err != nil {
		t.Fatalf("Update b: %v", err)
	}

	walPath := filepath.Join(dir, walName)
	staleWAL, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("ReadFile wal: %v", err)
	}
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 模拟“快照已 rename、WAL 尚未 truncate”时崩溃：把旧 WAL 放回去。
	// 旧记录早于快照代数，重放时必须整段跳过，否则唯一索引会报冲突。
	if err := os.WriteFile(walPath, staleWAL, filePerm); err != nil {
		t.Fatalf("WriteFile wal: %v", err)
	}

	reopened := openTestDB(t, dir)
	a, ok := reopened.Get("users", "a")
	if !ok || a["email"] != "z@example.com" {
		t.Fatalf("a = %v, %v", a, ok)
	}
	b, ok := reopened.Get("users", "b")
	if !ok || b["email"] != "x@example.com" {
		t.Fatalf("b = %v, %v", b, ok)
	}

	// 新写入属于新代数，必须能在旧代数记录之后正常重放。
	if _, err := reopened.Insert("users", map[string]any{"_id": "c", "email": "c@example.com"}); err != nil {
		t.Fatalf("Insert c: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}

	again, err := Open(dir, &Options{Sync: false})
	if err != nil {
		t.Fatalf("再次打开: %v", err)
	}
	defer again.Close()
	if _, ok := again.Get("users", "c"); !ok {
		t.Fatal("新代数的写入丢失")
	}
	if count, _ := again.Count("users", nil); count != 3 {
		t.Fatalf("文档数 = %d, want 3", count)
	}
}

func TestTornWALRecovery(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	if _, err := db.Insert("users", map[string]any{"name": "Alice"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	walPath := filepath.Join(dir, walName)
	f, err := os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	if _, err := f.WriteString(`{"op":"put","coll":"users","doc":{"_id":"torn"`); err != nil {
		t.Fatalf("write torn record: %v", err)
	}
	_ = f.Close()

	reopened := openTestDB(t, dir)
	if count, _ := reopened.Count("users", nil); count != 1 {
		t.Fatalf("撕裂 WAL 恢复后 %d 条，期望 1", count)
	}
	if _, ok := reopened.Get("users", "torn"); ok {
		t.Fatal("未完成的 WAL 记录不应生效")
	}
}

func TestCorruptWALFailsOpen(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	if _, err := db.Insert("users", map[string]any{"name": "Alice"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.OpenFile(filepath.Join(dir, walName), os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	if _, err := f.WriteString("this is not json\n"); err != nil {
		t.Fatalf("write corrupt record: %v", err)
	}
	_ = f.Close()

	if _, err := Open(dir, &Options{Sync: false}); err == nil {
		t.Fatal("损坏的完整 WAL 行应导致打开失败")
	}
}

func TestConcurrentInserts(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	const (
		workers = 8
		per     = 25
	)
	var wg sync.WaitGroup
	errCh := make(chan error, workers*per)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := db.Insert("events", map[string]any{"worker": w, "i": i}); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发插入失败: %v", err)
	}
	if count, _ := db.Count("events", nil); count != workers*per {
		t.Fatalf("并发插入后 %d 条，期望 %d", count, workers*per)
	}
}
