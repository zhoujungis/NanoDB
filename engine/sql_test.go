package engine

import (
	"errors"
	"testing"
)

func TestSQLSelectReadOnly(t *testing.T) {
	db := openTestDB(t, t.TempDir())

	docs := []map[string]any{
		{"_id": "a", "name": "Alice", "age": 30, "city": "Shanghai", "tags": []any{"go", "db"}},
		{"_id": "b", "name": "Bob", "age": 24, "city": "Beijing", "tags": []any{"go"}},
		{"_id": "c", "name": "Cara", "age": 42, "city": "Shanghai", "nickname": "cc"},
		{"_id": "d", "name": "Dan", "age": 18, "city": "Shenzhen", "active": true},
	}
	for _, doc := range docs {
		mustInsert(t, db, "users", doc)
	}

	run := func(stmt string) []map[string]any {
		t.Helper()
		out, err := db.QuerySQL(stmt)
		if err != nil {
			t.Fatalf("QuerySQL(%q): %v", stmt, err)
		}
		return out
	}
	ids := func(out []map[string]any) []string {
		t.Helper()
		res := make([]string, 0, len(out))
		for _, doc := range out {
			res = append(res, doc["_id"].(string))
		}
		return res
	}

	// 比较、AND/OR、括号。
	got := run(`SELECT name, age FROM users WHERE age >= 18 AND (city = 'Shanghai' OR city = 'Beijing') ORDER BY age DESC, name ASC`)
	if ids := ids(got); len(ids) != 3 || ids[0] != "c" || ids[1] != "a" || ids[2] != "b" {
		t.Fatalf("复合 WHERE 结果 = %v", ids)
	}
	if _, ok := got[0]["city"]; ok {
		t.Fatalf("投影不应包含 city: %v", got[0])
	}
	if got[0]["_id"] != "c" || got[0]["name"] != "Cara" {
		t.Fatalf("投影字段错误: %v", got[0])
	}

	// IN / NOT IN / BETWEEN / IS NULL / IS NOT NULL / NOT。
	if got := ids(run(`SELECT * FROM users WHERE name IN ('Alice', 'Bob')`)); len(got) != 2 {
		t.Fatalf("IN = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE name NOT IN ('Alice', 'Bob')`)); len(got) != 2 {
		t.Fatalf("NOT IN = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE age BETWEEN 20 AND 35`)); len(got) != 2 {
		t.Fatalf("BETWEEN = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE age NOT BETWEEN 20 AND 35`)); len(got) != 2 {
		t.Fatalf("NOT BETWEEN = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE nickname IS NULL`)); len(got) != 3 {
		t.Fatalf("IS NULL = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE nickname IS NOT NULL`)); len(got) != 1 || got[0] != "c" {
		t.Fatalf("IS NOT NULL = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE NOT (age > 30)`)); len(got) != 3 {
		t.Fatalf("NOT = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE city <> 'Shanghai'`)); len(got) != 2 {
		t.Fatalf("<> = %v", got)
	}

	// LIKE：% 与 _ 通配，正则元字符按字面处理。
	if got := ids(run(`SELECT * FROM users WHERE name LIKE '%li%'`)); len(got) != 1 || got[0] != "a" {
		t.Fatalf("LIKE %% = %v", got)
	}
	if got := ids(run(`SELECT * FROM users WHERE name LIKE '_ob'`)); len(got) != 1 || got[0] != "b" {
		t.Fatalf("LIKE _ = %v", got)
	}
	mustInsert(t, db, "users", map[string]any{"_id": "e", "name": "A.B", "age": 50})
	if got := ids(run(`SELECT * FROM users WHERE name LIKE 'A.B'`)); len(got) != 1 || got[0] != "e" {
		t.Fatalf("LIKE 字面点号 = %v", got)
	}

	// ORDER BY + LIMIT / OFFSET。
	if got := ids(run(`SELECT * FROM users ORDER BY age ASC LIMIT 2 OFFSET 1`)); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("LIMIT/OFFSET = %v", got)
	}
	if got := run(`SELECT * FROM users LIMIT 0`); len(got) != 0 {
		t.Fatalf("LIMIT 0 应返回空数组: %v", got)
	}

	// COUNT(*) 只返回一条计数。
	if got := run(`SELECT COUNT(*) FROM users WHERE age >= 30`); len(got) != 1 || got[0]["count"] != 3 {
		t.Fatalf("COUNT(*) = %v", got)
	}

	// 数组字段 multikey 语义。
	if got := ids(run(`SELECT * FROM users WHERE tags = 'db'`)); len(got) != 1 || got[0] != "a" {
		t.Fatalf("数组等值 = %v", got)
	}

	// 只读：写语句与非法语法都报 ErrInvalid。
	for _, stmt := range []string{
		`INSERT INTO users VALUES (1)`,
		`UPDATE users SET age = 1`,
		`DELETE FROM users`,
		`SELECT name AS n FROM users`,
		`SELECT COUNT(*), name FROM users`,
		`SELECT * FROM users LIMIT -1`,
		`SELECT * FROM users WHERE`,
		`SELECT * FROM users WHERE name = `,
		`SELECT * FROM users; SELECT * FROM users`,
		`SELECT * WHERE age = 1`,
	} {
		if _, err := db.QuerySQL(stmt); !errors.Is(err, ErrInvalid) {
			t.Fatalf("语句 %q 期望 ErrInvalid，得到 %v", stmt, err)
		}
	}
}

func TestSQLOnTransactionSnapshot(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	mustInsert(t, db, "users", map[string]any{"_id": "a", "name": "Alice", "age": 30})

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Insert("users", map[string]any{"_id": "b", "name": "Bob", "age": 20}); err != nil {
		t.Fatalf("tx Insert: %v", err)
	}
	got, err := RunSQL(tx, `SELECT COUNT(*) FROM users WHERE age >= 20`)
	if err != nil {
		t.Fatalf("RunSQL(tx): %v", err)
	}
	if len(got) != 1 || got[0]["count"] != 2 {
		t.Fatalf("事务内 COUNT = %v", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got, err := RunSQL(db, `SELECT COUNT(*) FROM users`); err != nil || got[0]["count"] != 1 {
		t.Fatalf("回滚后 COUNT = %v, %v", got, err)
	}
}
