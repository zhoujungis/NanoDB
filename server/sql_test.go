package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"nanodb/engine"
	"nanodb/server"
)

func TestHTTPSQLAndIndexTypes(t *testing.T) {
	ts := newTestServer(t)

	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken,
		`[{"_id":"a","name":"Alice","age":30,"city":"Shanghai"},{"_id":"b","name":"Bob","age":24,"city":"Beijing"},{"_id":"c","name":"Cara","age":42,"city":"Shanghai"}]`); status != http.StatusCreated {
		t.Fatalf("批量插入 = %d, body=%s", status, body)
	}

	status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/sql", testToken,
		`{"query":"SELECT name, age FROM users WHERE city = 'Shanghai' ORDER BY age DESC"}`)
	if status != http.StatusOK {
		t.Fatalf("SQL = %d, body=%s", status, body)
	}
	docs := decode[[]map[string]any](t, body)
	if len(docs) != 2 || docs[0]["name"] != "Cara" || docs[1]["name"] != "Alice" {
		t.Fatalf("SQL 结果 = %v", docs)
	}

	status, body = doRequest(t, http.MethodPost, ts.URL+"/v1/sql", testToken,
		`{"query":"SELECT COUNT(*) FROM users WHERE age >= 25"}`)
	if status != http.StatusOK || decode[[]map[string]any](t, body)[0]["count"].(float64) != 2 {
		t.Fatalf("SQL COUNT = %d, body=%s", status, body)
	}

	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/sql", testToken, `{"query":"DELETE FROM users"}`); status != http.StatusBadRequest {
		t.Fatalf("写语句 = %d, want 400", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/sql", testToken, `{"query":""}`); status != http.StatusBadRequest {
		t.Fatalf("空语句 = %d, want 400", status)
	}

	// 文本索引与 $text 查询。
	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/users", testToken,
		`{"field":"*","type":"text"}`); status != http.StatusCreated {
		t.Fatalf("创建文本索引 = %d, body=%s", status, body)
	}
	textFilter := `{"$text":{"$search":"alice"}}`
	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/users?filter="+url.QueryEscape(textFilter), testToken, "")
	if status != http.StatusOK {
		t.Fatalf("$text 查询 = %d, body=%s", status, body)
	}
	if got := decode[[]map[string]any](t, body); len(got) != 1 || got[0]["_id"] != "a" {
		t.Fatalf("$text 结果 = %v", got)
	}
	_, specBody := doRequest(t, http.MethodGet, ts.URL+"/v1/indexes/users", testToken, "")
	specs := decode[[]engine.IndexSpec](t, specBody)
	if len(specs) != 1 || specs[0].Type != engine.IndexTypeText {
		t.Fatalf("索引列表 = %+v", specs)
	}

	// 地理索引与 $geoWithin 查询。
	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/places", testToken,
		`[{"_id":"sh","location":[121.4737,31.2304]},{"_id":"bj","location":[116.4074,39.9042]}]`); status != http.StatusCreated {
		t.Fatalf("插入地点 = %d, body=%s", status, body)
	}
	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/places", testToken,
		`{"field":"location","type":"geo"}`); status != http.StatusCreated {
		t.Fatalf("创建地理索引 = %d, body=%s", status, body)
	}
	geoFilter := `{"location":{"$geoWithin":{"$circle":{"$center":[121.4737,31.2304],"$radius":50000}}}}`
	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/places?filter="+url.QueryEscape(geoFilter), testToken, "")
	if status != http.StatusOK {
		t.Fatalf("$geoWithin 查询 = %d, body=%s", status, body)
	}
	if got := decode[[]map[string]any](t, body); len(got) != 1 || got[0]["_id"] != "sh" {
		t.Fatalf("$geoWithin 结果 = %v", got)
	}
}

func TestHTTPSQLReadRoleACL(t *testing.T) {
	dir := t.TempDir()
	db, err := engine.Open(dir, &engine.Options{Sync: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Insert("users", map[string]any{"_id": "a", "name": "Alice"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	store, err := server.OpenUserStore(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("OpenUserStore: %v", err)
	}
	reader, err := store.Add("reader", server.RoleRead, []string{"users"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	ts := httptest.NewServer(server.NewWithOptions(db, server.Options{Users: store}).Handler())
	defer ts.Close()

	// read 角色可以执行只读 SQL。
	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/sql", reader.Token,
		`{"query":"SELECT name FROM users"}`); status != http.StatusOK {
		t.Fatalf("read SQL = %d, body=%s", status, body)
	}
	// 白名单外的集合拒绝。
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/sql", reader.Token,
		`{"query":"SELECT * FROM other"}`); status != http.StatusForbidden {
		t.Fatalf("read SQL 越权 = %d, want 403", status)
	}
	// 未鉴权仍然 401。
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/sql", "",
		`{"query":"SELECT * FROM users"}`); status != http.StatusUnauthorized {
		t.Fatalf("无凭据 SQL = %d, want 401", status)
	}
}
