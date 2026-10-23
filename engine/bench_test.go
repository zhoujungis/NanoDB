package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

const benchDocCount = 100000

// newBenchDB 生成基准数据集：100 个不同 age 值，每个值 1000 条文档。
func newBenchDB(b *testing.B, indexed bool) *DB {
	b.Helper()
	db, err := Open(b.TempDir(), &Options{Sync: false})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	ops := make([]BatchOp, 0, benchDocCount)
	for i := 0; i < benchDocCount; i++ {
		ops = append(ops, BatchOp{
			Op:   "insert",
			Coll: "bench",
			Doc: map[string]any{
				"_id":  fmt.Sprintf("id-%05d", i),
				"age":  i % 100,
				"name": fmt.Sprintf("user-%05d", i),
			},
		})
	}
	if _, err := db.Batch(ops); err != nil {
		b.Fatalf("seed: %v", err)
	}
	if indexed {
		if err := db.CreateIndex("bench", "age", false); err != nil {
			b.Fatalf("CreateIndex: %v", err)
		}
	}
	return db
}

func runFindBench(b *testing.B, db *DB, q Query, want int) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		docs, err := db.Find("bench", q)
		if err != nil {
			b.Fatalf("Find: %v", err)
		}
		if len(docs) != want {
			b.Fatalf("Find 返回 %d 条, want %d", len(docs), want)
		}
	}
}

// 等值查询：全表扫描作为基线。
func BenchmarkFindEqualityScan(b *testing.B) {
	db := newBenchDB(b, false)
	runFindBench(b, db, Query{Filter: map[string]any{"age": 42}}, benchDocCount/100)
}

// 等值查询：命中二级索引。
func BenchmarkFindEqualityIndexed(b *testing.B) {
	db := newBenchDB(b, true)
	runFindBench(b, db, Query{Filter: map[string]any{"age": 42}}, benchDocCount/100)
}

// 范围查询：全表扫描作为基线。
func BenchmarkFindRangeScan(b *testing.B) {
	db := newBenchDB(b, false)
	runFindBench(b, db, Query{Filter: map[string]any{"age": map[string]any{"$gte": 99}}}, benchDocCount/100)
}

// 范围查询：命中二级索引，验证二分定位的开销。
func BenchmarkFindRangeIndexed(b *testing.B) {
	db := newBenchDB(b, true)
	runFindBench(b, db, Query{Filter: map[string]any{"age": map[string]any{"$gte": 99}}}, benchDocCount/100)
}

// 投影查询：命中索引，只返回 name 字段，衡量结果集拷贝的开销。
func BenchmarkFindProjectedIndexed(b *testing.B) {
	db := newBenchDB(b, true)
	runFindBench(b, db, Query{
		Filter: map[string]any{"age": 42},
		Fields: []string{"name"},
	}, benchDocCount/100)
}

// 排序：无索引可用，10 万条全量匹配后排序并取前 50 条。
func BenchmarkFindSortScan(b *testing.B) {
	db := newBenchDB(b, false)
	runFindBench(b, db, Query{
		Filter: map[string]any{"age": map[string]any{"$gte": 0}},
		Sort:   []SortField{{Field: "name", Desc: true}},
		Limit:  50,
	}, 50)
}

// 分页：命中索引的 1 万条结果上跳过 500 条取 50 条，不排序。
func BenchmarkFindPageIndexed(b *testing.B) {
	db := newBenchDB(b, true)
	runFindBench(b, db, Query{
		Filter: map[string]any{"age": map[string]any{"$gte": 90}},
		Skip:   500,
		Limit:  50,
	}, 50)
}

// 排序 + 分页：命中索引后在 1 万条结果上排序，再跳过 500 条取 50 条。
func BenchmarkFindSortPageIndexed(b *testing.B) {
	db := newBenchDB(b, true)
	runFindBench(b, db, Query{
		Filter: map[string]any{"age": map[string]any{"$gte": 90}},
		Sort:   []SortField{{Field: "name", Desc: true}},
		Skip:   500,
		Limit:  50,
	}, 50)
}

// randomBenchDocs 生成按随机顺序排列的文档，避免输入已有序导致排序走最优路径。
func randomBenchDocs(n int) []map[string]any {
	perm := rand.New(rand.NewSource(1)).Perm(n)
	docs := make([]map[string]any, n)
	for i, j := range perm {
		docs[i] = map[string]any{
			"_id":  fmt.Sprintf("id-%06d", i),
			"age":  j % 100,
			"name": fmt.Sprintf("user-%06d", j),
		}
	}
	return docs
}

func runSortAB(b *testing.B, topN bool) {
	docs := randomBenchDocs(benchDocCount)
	order := []SortField{{Field: "name", Desc: true}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		input := append([]map[string]any(nil), docs...)
		if topN {
			topNSortDocs(input, order, 50)
		} else {
			sortDocs(input, order)
		}
	}
}

// 随机序数据集上的排序 A/B：全量排序。
func BenchmarkSortRandomFull(b *testing.B) {
	runSortAB(b, false)
}

// 随机序数据集上的排序 A/B：Top-50。
func BenchmarkSortRandomTopN(b *testing.B) {
	runSortAB(b, true)
}
