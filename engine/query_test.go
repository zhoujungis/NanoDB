package engine

import (
	"fmt"
	"testing"
)

func TestMatchFilter(t *testing.T) {
	doc := map[string]any{
		"name":   "Alice",
		"age":    30,
		"active": true,
		"tags":   []any{"go", "db"},
		"profile": map[string]any{
			"city": "Shanghai",
		},
		"score": nil,
	}

	cases := []struct {
		name   string
		filter map[string]any
		want   bool
	}{
		{"等值字符串", map[string]any{"name": "Alice"}, true},
		{"等值数字 int 与 float", map[string]any{"age": 30.0}, true},
		{"嵌套对象等值", map[string]any{"profile": map[string]any{"city": "Shanghai"}}, true},
		{"$gt", map[string]any{"age": map[string]any{"$gt": 29}}, true},
		{"$gte 不满足", map[string]any{"age": map[string]any{"$gte": 31}}, false},
		{"$lt 不满足", map[string]any{"age": map[string]any{"$lt": 30}}, false},
		{"$ne", map[string]any{"name": map[string]any{"$ne": "Bob"}}, true},
		{"$in", map[string]any{"name": map[string]any{"$in": []any{"Bob", "Alice"}}}, true},
		{"$nin", map[string]any{"name": map[string]any{"$nin": []any{"Bob"}}}, true},
		{"$exists true", map[string]any{"profile.city": map[string]any{"$exists": true}}, true},
		{"$exists false", map[string]any{"missing": map[string]any{"$exists": false}}, true},
		{"$regex", map[string]any{"name": map[string]any{"$regex": "^Ali"}}, true},
		{"数组包含标量", map[string]any{"tags": "go"}, true},
		{"数组整体等值", map[string]any{"tags": []any{"go", "db"}}, true},
		{"$and", map[string]any{"$and": []any{
			map[string]any{"age": map[string]any{"$gt": 20}},
			map[string]any{"name": "Alice"},
		}}, true},
		{"$or", map[string]any{"$or": []any{
			map[string]any{"name": "Bob"},
			map[string]any{"age": 30},
		}}, true},
		{"$not", map[string]any{"$not": map[string]any{"name": "Bob"}}, true},
		{"点路径", map[string]any{"profile.city": "Shanghai"}, true},
		{"null 等值", map[string]any{"score": nil}, true},
		{"缺失字段等值", map[string]any{"nope": "x"}, false},
		{"布尔等值", map[string]any{"active": true}, true},
		{"$in 未命中", map[string]any{"name": map[string]any{"$in": []any{"Bob"}}}, false},
		{"$regex 未命中", map[string]any{"name": map[string]any{"$regex": "^Bob"}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchFilter(doc, tc.filter); got != tc.want {
				t.Fatalf("matchFilter(%v) = %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
}

func TestParseSort(t *testing.T) {
	fields, err := ParseSort("age:-1,name:1")
	if err != nil {
		t.Fatalf("ParseSort: %v", err)
	}
	if len(fields) != 2 || fields[0].Field != "age" || !fields[0].Desc || fields[1].Field != "name" || fields[1].Desc {
		t.Fatalf("解析结果错误: %+v", fields)
	}
	if _, err := ParseSort("age:desc,name:asc"); err != nil {
		t.Fatalf("ParseSort: %v", err)
	}
	if _, err := ParseSort("age:up"); err == nil {
		t.Fatal("非法排序方向应报错")
	}
	if _, err := ParseSort(":1"); err == nil {
		t.Fatal("空字段名应报错")
	}
}

func TestTopNSortMatchesFullSort(t *testing.T) {
	docs := make([]map[string]any, 0, 120)
	for i := 0; i < 120; i++ {
		doc := map[string]any{
			"_id":  fmt.Sprintf("d%03d", i),
			"name": fmt.Sprintf("n%03d", (i*37)%120),
			"age":  i % 5,
		}
		if i%7 == 0 {
			delete(doc, "age")
		}
		docs = append(docs, doc)
	}

	orders := [][]SortField{
		{{Field: "age"}},
		{{Field: "age", Desc: true}},
		{{Field: "age"}, {Field: "name", Desc: true}},
		{{Field: "missing"}},
	}
	for _, fields := range orders {
		want := append([]map[string]any(nil), docs...)
		sortDocs(want, fields)
		for _, k := range []int{1, 3, 50, len(docs), len(docs) + 5} {
			got := topNSortDocs(append([]map[string]any(nil), docs...), fields, k)
			n := k
			if n > len(docs) {
				n = len(docs)
			}
			if len(got) != n {
				t.Fatalf("sort=%v k=%d: 返回 %d 条, want %d", fields, k, len(got), n)
			}
			for i := 0; i < n; i++ {
				if got[i]["_id"] != want[i]["_id"] {
					t.Fatalf("sort=%v k=%d: 第 %d 条为 %v, want %v", fields, k, i, got[i]["_id"], want[i]["_id"])
				}
			}
		}
	}
}

func TestFindPreparedRegex(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	for _, name := range []string{"Alice", "Alicia", "Bob"} {
		if _, err := db.Insert("users", map[string]any{"name": name}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	docs, err := db.Find("users", Query{Filter: map[string]any{"name": map[string]any{"$regex": "^Ali"}}})
	if err != nil || len(docs) != 2 {
		t.Fatalf("$regex 查询 = %v, %v, want 2", docs, err)
	}

	// 嵌套在 $and 中的正则同样在查询前预编译。
	docs, err = db.Find("users", Query{Filter: map[string]any{"$and": []any{
		map[string]any{"name": map[string]any{"$regex": "^Bob"}},
	}}})
	if err != nil || len(docs) != 1 {
		t.Fatalf("$and $regex 查询 = %v, %v, want 1", docs, err)
	}

	// 非法模式视为不命中，而不是报错或逐条文档重复编译。
	docs, err = db.Find("users", Query{Filter: map[string]any{"name": map[string]any{"$regex": "("}}})
	if err != nil || len(docs) != 0 {
		t.Fatalf("非法 $regex 查询 = %v, %v, want 0", docs, err)
	}
}
