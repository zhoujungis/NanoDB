package engine

import (
	"errors"
	"testing"
)

func mustInsert(t *testing.T, db *DB, coll string, doc map[string]any) {
	t.Helper()
	if _, err := db.Insert(coll, doc); err != nil {
		t.Fatalf("Insert(%q, %v): %v", coll, doc["_id"], err)
	}
}

func findIDs(t *testing.T, db *DB, coll string, filter map[string]any) []string {
	t.Helper()
	docs, err := db.Find(coll, Query{Filter: filter})
	if err != nil {
		t.Fatalf("Find(%v): %v", filter, err)
	}
	ids := make([]string, 0, len(docs))
	for _, doc := range docs {
		ids = append(ids, doc["_id"].(string))
	}
	return ids
}

func TestTextIndexQuery(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	mustInsert(t, db, "articles", map[string]any{
		"_id": "a", "name": "Alice", "bio": "Go developer", "tags": []any{"database", "search"},
	})
	mustInsert(t, db, "articles", map[string]any{
		"_id": "b", "name": "Bob", "bio": "Rust developer",
	})
	mustInsert(t, db, "articles", map[string]any{
		"_id": "c", "name": "张三", "bio": "数据库工程师",
	})

	textQuery := func(search string, path string) map[string]any {
		cond := map[string]any{"$search": search}
		if path != "" {
			cond["$path"] = path
		}
		return map[string]any{"$text": cond}
	}

	// 建索引前也必须能查：$text 不依赖索引做语义判断。
	if got := findIDs(t, db, "articles", textQuery("alice developer", "")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("建索引前 $text = %v, want [a]", got)
	}

	if err := db.CreateIndexSpec("articles", IndexSpec{Field: "*", Type: IndexTypeText}); err != nil {
		t.Fatalf("CreateIndexSpec text: %v", err)
	}
	if specs := db.ListIndexes("articles"); len(specs) != 1 || specs[0].Type != IndexTypeText || specs[0].Field != "*" {
		t.Fatalf("ListIndexes = %+v", specs)
	}

	// 多个词元之间是 AND，且能命中嵌套与数组里的字符串。
	if got := findIDs(t, db, "articles", textQuery("alice developer", "")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("$text AND = %v", got)
	}
	if got := findIDs(t, db, "articles", textQuery("database search", "")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("$text 数组字段 = %v", got)
	}
	if got := findIDs(t, db, "articles", textQuery("alice zzz", "")); len(got) != 0 {
		t.Fatalf("不存在的词元不应命中: %v", got)
	}

	// $path 限定字段范围。
	if got := findIDs(t, db, "articles", textQuery("alice", "name")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("$path=name = %v", got)
	}
	if got := findIDs(t, db, "articles", textQuery("alice", "bio")); len(got) != 0 {
		t.Fatalf("$path=bio 不应命中: %v", got)
	}

	// 连续汉字按二元组匹配。
	if got := findIDs(t, db, "articles", textQuery("数据库", "")); len(got) != 1 || got[0] != "c" {
		t.Fatalf("中文 $text = %v", got)
	}

	// 更新后旧词元失效、新词元生效。
	if _, err := db.Update("articles", "a", map[string]any{"$set": map[string]any{"bio": "Rust developer"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := findIDs(t, db, "articles", textQuery("go", "")); len(got) != 0 {
		t.Fatalf("更新后旧词元仍命中: %v", got)
	}
	if got := findIDs(t, db, "articles", textQuery("rust developer", "")); len(got) != 2 {
		t.Fatalf("更新后新词元 = %v, want 2 条", got)
	}

	// 删除同步维护索引。
	if ok, err := db.Delete("articles", "b"); err != nil || !ok {
		t.Fatalf("Delete: %v, %v", err, ok)
	}
	if got := findIDs(t, db, "articles", textQuery("rust", "")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("删除后 $text = %v", got)
	}

	// 压缩成快照后重开，文本索引定义与数据都要恢复。
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(dir, &Options{Sync: false})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := findIDs(t, db2, "articles", textQuery("rust", "")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("重开后 $text = %v", got)
	}

	// 非法定义。
	if err := db2.CreateIndexSpec("articles", IndexSpec{Field: "name", Type: IndexTypeText}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("文本索引字段必须为 *，得到 %v", err)
	}
	if err := db2.CreateIndexSpec("articles", IndexSpec{Field: "*", Type: IndexTypeText, Unique: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("文本索引不能唯一，得到 %v", err)
	}
	if err := db2.CreateIndexSpec("articles", IndexSpec{Field: "name", Type: "bogus"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未知索引类型应报错，得到 %v", err)
	}
}

func TestGeoIndexQuery(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	mustInsert(t, db, "places", map[string]any{"_id": "sh", "name": "Shanghai", "location": []any{121.4737, 31.2304}})
	mustInsert(t, db, "places", map[string]any{"_id": "sz", "name": "Suzhou", "location": []any{120.5853, 31.2989}})
	mustInsert(t, db, "places", map[string]any{"_id": "bj", "name": "Beijing", "location": []any{116.4074, 39.9042}})
	mustInsert(t, db, "places", map[string]any{
		"_id": "geo", "name": "GeoJSON",
		"location": map[string]any{"type": "Point", "coordinates": []any{121.50, 31.24}},
	})
	mustInsert(t, db, "places", map[string]any{"_id": "bad", "name": "Bad", "location": []any{"x", 1}})

	box := map[string]any{"$geoWithin": map[string]any{"$box": []any{
		[]any{120.0, 30.0}, []any{122.0, 32.0},
	}}}
	circle := func(radius float64) map[string]any {
		return map[string]any{"$geoWithin": map[string]any{"$circle": map[string]any{
			"$center": []any{121.4737, 31.2304}, "$radius": radius,
		}}}
	}

	// 未建索引时也按同样的语义全表匹配。
	if got := findIDs(t, db, "places", map[string]any{"location": box}); len(got) != 3 {
		t.Fatalf("建索引前 $box = %v, want 3 条", got)
	}

	if err := db.CreateIndexSpec("places", IndexSpec{Field: "location", Type: IndexTypeGeo}); err != nil {
		t.Fatalf("CreateIndexSpec geo: %v", err)
	}

	if got := findIDs(t, db, "places", map[string]any{"location": box}); len(got) != 3 {
		t.Fatalf("$box = %v, want 3 条", got)
	}
	if got := findIDs(t, db, "places", map[string]any{"location": circle(100000)}); len(got) != 3 {
		t.Fatalf("100km 圆 = %v, want 3 条", got)
	}
	if got := findIDs(t, db, "places", map[string]any{"location": circle(10000)}); len(got) != 2 {
		t.Fatalf("10km 圆 = %v, want 2 条", got)
	}
	if got := findIDs(t, db, "places", map[string]any{"location": box, "name": "Suzhou"}); len(got) != 1 || got[0] != "sz" {
		t.Fatalf("地理 + 普通条件 = %v", got)
	}

	// 移动地点后索引同步更新。
	if _, err := db.Update("places", "sz", map[string]any{"$set": map[string]any{"location": []any{116.4074, 39.9042}}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := findIDs(t, db, "places", map[string]any{"location": circle(100000)}); len(got) != 2 {
		t.Fatalf("移动后 100km 圆 = %v, want 2 条", got)
	}

	// 删除同步维护索引。
	if ok, err := db.Delete("places", "sh"); err != nil || !ok {
		t.Fatalf("Delete: %v, %v", err, ok)
	}
	if got := findIDs(t, db, "places", map[string]any{"location": circle(100000)}); len(got) != 1 || got[0] != "geo" {
		t.Fatalf("删除后圆查询 = %v", got)
	}

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(dir, &Options{Sync: false})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := findIDs(t, db2, "places", map[string]any{"location": circle(100000)}); len(got) != 1 || got[0] != "geo" {
		t.Fatalf("重开后圆查询 = %v", got)
	}

	if err := db2.CreateIndexSpec("places", IndexSpec{Fields: []string{"location", "name"}, Type: IndexTypeGeo}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("地理索引必须单字段，得到 %v", err)
	}
	if err := db2.CreateIndexSpec("places", IndexSpec{Field: "name", Type: IndexTypeGeo, Unique: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("地理索引不能唯一，得到 %v", err)
	}
}
