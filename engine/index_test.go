package engine

import (
	"errors"
	"math"
	"testing"
)

func TestIndexNegativeZeroEquality(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	negZero := math.Copysign(0, -1)
	if _, err := db.Insert("metrics", map[string]any{"_id": "neg", "value": negZero}); err != nil {
		t.Fatalf("Insert neg: %v", err)
	}
	if _, err := db.Insert("metrics", map[string]any{"_id": "pos", "value": 0.0}); err != nil {
		t.Fatalf("Insert pos: %v", err)
	}
	if err := db.CreateIndex("metrics", "value", false); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	// 0 与 -0 按数值相等，等值查询必须同时命中两条，而不是只命中一条。
	for _, v := range []float64{0, negZero} {
		got, err := db.Find("metrics", Query{Filter: map[string]any{"value": v}})
		if err != nil {
			t.Fatalf("Find(%v): %v", v, err)
		}
		if len(got) != 2 {
			t.Fatalf("Find(%v) 返回 %d 条，want 2", v, len(got))
		}
	}

	if _, err := db.Insert("uniq", map[string]any{"_id": "neg", "value": negZero}); err != nil {
		t.Fatalf("Insert uniq neg: %v", err)
	}
	if err := db.CreateIndex("uniq", "value", true); err != nil {
		t.Fatalf("CreateIndex unique: %v", err)
	}
	if _, err := db.Insert("uniq", map[string]any{"_id": "pos", "value": 0.0}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("0 与 -0 应视为重复唯一键，得到 %v", err)
	}
}

func TestSecondaryIndexQueryAndPersistence(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	docs := []map[string]any{
		{"_id": "u10", "name": "Alice", "age": 10, "tags": []any{"go", "db"}, "profile": map[string]any{"city": "Shanghai"}},
		{"_id": "u20", "name": "Bob", "age": 20, "tags": []any{"db"}, "profile": map[string]any{"city": "Beijing"}},
		{"_id": "u30", "name": "Cara", "age": 30, "tags": []any{"go"}, "profile": map[string]any{"city": "Shanghai"}},
		{"_id": "u40", "name": "Dan", "age": 40},
	}
	for _, doc := range docs {
		if _, err := db.Insert("users", doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	for _, spec := range []struct {
		field  string
		unique bool
	}{
		{"age", false},
		{"profile.city", false},
		{"tags", false},
	} {
		if err := db.CreateIndex("users", spec.field, spec.unique); err != nil {
			t.Fatalf("CreateIndex(%s): %v", spec.field, err)
		}
	}
	if specs := db.ListIndexes("users"); len(specs) != 3 {
		t.Fatalf("索引数量 = %d, want 3", len(specs))
	}

	rangeFilter := map[string]any{"age": map[string]any{"$gte": 30}}
	got, err := db.Find("users", Query{Filter: rangeFilter})
	if err != nil || len(got) != 2 {
		t.Fatalf("范围查询 = %v, %v, want 2", got, err)
	}
	if c := db.colls["users"]; c != nil {
		cands := c.planCandidates(rangeFilter)
		if len(cands) != 2 {
			t.Fatalf("索引候选集大小 = %d, want 2", len(cands))
		}
	}

	got, err = db.Find("users", Query{Filter: map[string]any{"profile.city": "Shanghai"}})
	if err != nil || len(got) != 2 {
		t.Fatalf("嵌套字段索引查询失败: %v, %v", got, err)
	}
	got, err = db.Find("users", Query{Filter: map[string]any{"tags": "go"}})
	if err != nil || len(got) != 2 {
		t.Fatalf("数组索引查询失败: %v, %v", got, err)
	}
	got, err = db.Find("users", Query{Filter: map[string]any{"age": map[string]any{"$in": []any{20, 40}}}})
	if err != nil || len(got) != 2 {
		t.Fatalf("$in 索引查询失败: %v, %v", got, err)
	}

	// 更新索引字段后，索引与查询结果必须同步。
	if _, err := db.Update("users", "u10", map[string]any{"$set": map[string]any{"age": 50}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = db.Find("users", Query{Filter: map[string]any{"age": 50}})
	if len(got) != 1 || got[0]["_id"] != "u10" {
		t.Fatalf("更新后索引查询错误: %v", got)
	}
	got, _ = db.Find("users", Query{Filter: map[string]any{"age": map[string]any{"$lte": 10}}})
	if len(got) != 0 {
		t.Fatalf("旧索引值仍可查到文档: %v", got)
	}

	// 删除文档后索引条目应清理。
	if _, err := db.Delete("users", "u20"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n, _ := db.Count("users", map[string]any{"age": 20}); n != 0 {
		t.Fatalf("删除后按索引统计 = %d, want 0", n)
	}

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openTestDB(t, dir)
	if specs := reopened.ListIndexes("users"); len(specs) != 3 {
		t.Fatalf("重启后索引数量 = %d, want 3", len(specs))
	}
	got, _ = reopened.Find("users", Query{Filter: map[string]any{"age": map[string]any{"$gte": 30}}})
	if len(got) != 3 {
		t.Fatalf("重启后索引查询 = %v", got)
	}

	if err := reopened.DropIndex("users", "age"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if specs := reopened.ListIndexes("users"); len(specs) != 2 {
		t.Fatalf("删除索引后数量 = %d, want 2", len(specs))
	}
	got, _ = reopened.Find("users", Query{Filter: map[string]any{"age": map[string]any{"$gte": 30}}})
	if len(got) != 3 {
		t.Fatalf("删除索引后查询结果错误: %v", got)
	}
	if stats := reopened.Stats(); stats.Indexes != 2 {
		t.Fatalf("stats.Indexes = %d, want 2", stats.Indexes)
	}
}

func TestUniqueIndex(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	for _, doc := range []map[string]any{
		{"_id": "a", "email": "a@example.com", "name": "A"},
		{"_id": "b", "email": "b@example.com", "name": "B"},
	} {
		if _, err := db.Insert("users", doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if err := db.CreateIndex("users", "email", true); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	if _, err := db.Insert("users", map[string]any{"email": "a@example.com"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("唯一索引冲突插入 = %v, want ErrDuplicateKey", err)
	}
	if _, err := db.Update("users", "b", map[string]any{"$set": map[string]any{"email": "a@example.com"}}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("唯一索引冲突更新 = %v, want ErrDuplicateKey", err)
	}
	got, ok := db.Get("users", "b")
	if !ok || got["email"] != "b@example.com" {
		t.Fatalf("冲突更新后文档被破坏: %v", got)
	}
	if _, err := db.Replace("users", "b", map[string]any{"email": "a@example.com"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("唯一索引冲突替换 = %v, want ErrDuplicateKey", err)
	}

	if _, err := db.Delete("users", "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.Insert("users", map[string]any{"_id": "c", "email": "a@example.com"}); err != nil {
		t.Fatalf("删除后复用唯一值失败: %v", err)
	}

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openTestDB(t, dir)
	if _, err := reopened.Insert("users", map[string]any{"email": "a@example.com"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("重启后唯一索引失效: %v", err)
	}
}

func TestCreateUniqueIndexWithDuplicatesFails(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	for _, id := range []string{"a", "b"} {
		if _, err := db.Insert("users", map[string]any{"_id": id, "email": "same@example.com"}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if err := db.CreateIndex("users", "email", true); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("重复数据建唯一索引 = %v, want ErrDuplicateKey", err)
	}
	if specs := db.ListIndexes("users"); len(specs) != 0 {
		t.Fatalf("失败后不应注册索引: %+v", specs)
	}
}

func TestIndexRangeBounds(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	for _, doc := range []map[string]any{
		{"_id": "a", "score": 1},
		{"_id": "b", "score": 2},
		{"_id": "c", "score": 2},
		{"_id": "d", "score": 3},
		{"_id": "e", "name": "missing-score"},
	} {
		if _, err := db.Insert("items", doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if err := db.CreateIndex("items", "score", false); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	cases := []struct {
		op   string
		val  any
		want []string
	}{
		{"$gt", 2, []string{"d"}},
		{"$gte", 2, []string{"b", "c", "d"}},
		{"$lt", 2, []string{"a"}},
		{"$lte", 2, []string{"a", "b", "c"}},
		{"$gt", 3, nil},
		{"$lte", 0, nil},
	}
	for _, tc := range cases {
		filter := map[string]any{"score": map[string]any{tc.op: tc.val}}
		got, err := db.Find("items", Query{Filter: filter})
		if err != nil {
			t.Fatalf("%s %v: %v", tc.op, tc.val, err)
		}
		var ids []string
		for _, doc := range got {
			ids = append(ids, doc["_id"].(string))
		}
		if len(ids) != len(tc.want) {
			t.Fatalf("%s %v 结果 = %v, want %v", tc.op, tc.val, ids, tc.want)
		}
		for i := range ids {
			if ids[i] != tc.want[i] {
				t.Fatalf("%s %v 结果 = %v, want %v", tc.op, tc.val, ids, tc.want)
			}
		}
		if cands := db.colls["items"].planCandidates(filter); len(cands) != len(tc.want) {
			t.Fatalf("%s %v 候选集大小 = %d, want %d", tc.op, tc.val, len(cands), len(tc.want))
		}
	}
}
