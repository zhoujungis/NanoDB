package engine

import (
	"errors"
	"testing"
)

func TestAggregatePipeline(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	docs := []map[string]any{
		{"_id": "a", "city": "SH", "age": 20, "tags": []any{"go", "db"}},
		{"_id": "b", "city": "BJ", "age": 30, "tags": []any{"go"}},
		{"_id": "c", "city": "SH", "age": 40, "tags": []any{"db"}},
	}
	for _, doc := range docs {
		if _, err := db.Insert("users", doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	got, err := db.Aggregate("users", []map[string]any{
		{"$group": map[string]any{
			"_id": "$city",
			"n":   map[string]any{"$sum": 1},
			"avg": map[string]any{"$avg": "$age"},
			"ids": map[string]any{"$push": "$_id"},
		}},
		{"$sort": []any{map[string]any{"_id": 1}}},
	})
	if err != nil {
		t.Fatalf("Aggregate group: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("分组数量 = %d, want 2", len(got))
	}
	if got[0]["_id"] != "BJ" || got[0]["n"] != float64(1) || got[0]["avg"] != float64(30) {
		t.Fatalf("BJ 分组 = %+v", got[0])
	}
	if got[1]["_id"] != "SH" || got[1]["n"] != float64(2) || got[1]["avg"] != float64(30) {
		t.Fatalf("SH 分组 = %+v", got[1])
	}
	if ids, ok := got[1]["ids"].([]any); !ok || len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Fatalf("SH ids = %+v", got[1]["ids"])
	}

	// $match + $unwind + $group + $sort + $limit + $project
	got, err = db.Aggregate("users", []map[string]any{
		{"$match": map[string]any{"age": map[string]any{"$gte": 20}}},
		{"$unwind": "$tags"},
		{"$group": map[string]any{"_id": "$tags", "n": map[string]any{"$sum": 1}}},
		{"$sort": map[string]any{"n": -1}},
		{"$limit": 2},
		{"$project": map[string]any{"_id": 1, "n": 1}},
	})
	if err != nil {
		t.Fatalf("Aggregate unwind: %v", err)
	}
	if len(got) != 2 || got[0]["_id"] != "go" || got[0]["n"] != float64(2) || got[1]["_id"] != "db" {
		t.Fatalf("unwind 结果 = %+v", got)
	}

	// 排除式 $project + $skip + $count
	got, err = db.Aggregate("users", []map[string]any{
		{"$sort": map[string]any{"age": 1}},
		{"$skip": 1},
		{"$project": map[string]any{"tags": 0, "city": 0}},
	})
	if err != nil {
		t.Fatalf("Aggregate project: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("skip 后数量 = %d, want 2", len(got))
	}
	if _, ok := got[0]["tags"]; ok {
		t.Fatalf("应排除 tags: %+v", got[0])
	}
	if _, ok := got[0]["city"]; ok {
		t.Fatalf("应排除 city: %+v", got[0])
	}

	got, err = db.Aggregate("users", []map[string]any{{"$count": "total"}})
	if err != nil || len(got) != 1 || got[0]["total"] != float64(3) {
		t.Fatalf("$count = %+v, %v", got, err)
	}

	// 非法阶段
	if _, err := db.Aggregate("users", []map[string]any{{"$lookup": map[string]any{}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未知阶段期望 ErrInvalid，得到 %v", err)
	}
}
