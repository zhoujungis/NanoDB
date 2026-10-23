package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"nanodb/engine"
	"nanodb/server"
)

const testToken = "secret"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := engine.Open(t.TempDir(), &engine.Options{Sync: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ts := httptest.NewServer(server.New(db, testToken).Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = db.Close()
	})
	return ts
}

func doRequest(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, data
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("解析响应失败: %v, body=%s", err, data)
	}
	return v
}

func TestHTTPCRUD(t *testing.T) {
	ts := newTestServer(t)

	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/healthz", "", ""); status != http.StatusOK {
		t.Fatalf("healthz = %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/collections", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("无令牌访问 = %d, want 401", status)
	}

	status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, `{"name":"Alice","age":30}`)
	if status != http.StatusCreated {
		t.Fatalf("插入 = %d, body=%s", status, body)
	}
	alice := decode[map[string]any](t, body)
	id := alice["_id"].(string)

	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/users/"+id, testToken, "")
	if status != http.StatusOK {
		t.Fatalf("读取 = %d, body=%s", status, body)
	}

	status, body = doRequest(t, http.MethodPatch, ts.URL+"/v1/users/"+id, testToken, `{"$inc":{"age":1}}`)
	if status != http.StatusOK {
		t.Fatalf("更新 = %d, body=%s", status, body)
	}
	if got := decode[map[string]any](t, body); got["age"].(float64) != 31 {
		t.Fatalf("更新后 age = %v", got["age"])
	}

	status, body = doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken,
		`[{"name":"Bob","age":18},{"name":"Cara","age":42}]`)
	if status != http.StatusCreated {
		t.Fatalf("批量插入 = %d, body=%s", status, body)
	}
	if got := decode[map[string]any](t, body); got["inserted"].(float64) != 2 {
		t.Fatalf("批量插入数量 = %v", got["inserted"])
	}

	filter := url.QueryEscape(`{"age":{"$gte":30}}`)
	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/users?filter="+filter+"&sort=age:-1&limit=5", testToken, "")
	if status != http.StatusOK {
		t.Fatalf("查询 = %d, body=%s", status, body)
	}
	docs := decode[[]map[string]any](t, body)
	if len(docs) != 2 || docs[0]["name"] != "Cara" || docs[1]["name"] != "Alice" {
		t.Fatalf("查询结果错误: %v", docs)
	}

	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/users?count=1&filter="+filter, testToken, "")
	if status != http.StatusOK || decode[map[string]any](t, body)["count"].(float64) != 2 {
		t.Fatalf("count = %d, body=%s", status, body)
	}

	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/users?fields=name", testToken, "")
	if status != http.StatusOK {
		t.Fatalf("投影查询 = %d", status)
	}
	for _, doc := range decode[[]map[string]any](t, body) {
		if _, ok := doc["age"]; ok {
			t.Fatalf("投影不应包含 age: %v", doc)
		}
	}

	status, _ = doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, `{"_id":"`+id+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("重复 _id = %d, want 409", status)
	}
	status, _ = doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, `{"name":`)
	if status != http.StatusBadRequest {
		t.Fatalf("非法 JSON = %d, want 400", status)
	}

	status, body = doRequest(t, http.MethodPut, ts.URL+"/v1/users/"+id, testToken, `{"name":"Alice","age":50,"city":"Shanghai"}`)
	if status != http.StatusOK {
		t.Fatalf("替换 = %d, body=%s", status, body)
	}
	if got := decode[map[string]any](t, body); got["_id"] != id || got["city"] != "Shanghai" {
		t.Fatalf("替换结果错误: %v", got)
	}

	status, body = doRequest(t, http.MethodDelete, ts.URL+"/v1/users/"+id, testToken, "")
	if status != http.StatusOK {
		t.Fatalf("删除 = %d, body=%s", status, body)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users/"+id, testToken, ""); status != http.StatusNotFound {
		t.Fatalf("删除后读取 = %d, want 404", status)
	}

	youngFilter := url.QueryEscape(`{"age":{"$lt":20}}`)
	status, body = doRequest(t, http.MethodDelete, ts.URL+"/v1/users?filter="+youngFilter, testToken, "")
	if status != http.StatusOK || decode[map[string]any](t, body)["deleted"].(float64) != 1 {
		t.Fatalf("按条件删除 = %d, body=%s", status, body)
	}

	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/collections", testToken, "")
	if status != http.StatusOK {
		t.Fatalf("集合列表 = %d", status)
	}
	cols := decode[[]engine.CollectionInfo](t, body)
	if len(cols) != 1 || cols[0].Name != "users" || cols[0].Count != 1 {
		t.Fatalf("集合信息错误: %+v", cols)
	}

	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/admin/stats", testToken, "")
	if status != http.StatusOK || decode[engine.Stats](t, body).Documents != 1 {
		t.Fatalf("stats = %d, body=%s", status, body)
	}

	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/admin/compact", testToken, ""); status != http.StatusOK {
		t.Fatalf("compact = %d, body=%s", status, body)
	}

	status, body = doRequest(t, http.MethodDelete, ts.URL+"/v1/collections/users", testToken, "")
	if status != http.StatusOK {
		t.Fatalf("删除集合 = %d, body=%s", status, body)
	}

	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users", testToken, ""); status != http.StatusOK {
		t.Fatalf("空集合查询 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/unknown/path/x", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("未知路径 = %d, want 404", status)
	}
	if status, _ := doRequest(t, http.MethodPut, ts.URL+"/v1/users", testToken, `{}`); status != http.StatusMethodNotAllowed {
		t.Fatalf("不支持的方法 = %d, want 405", status)
	}
}

func TestHTTPTokenHeader(t *testing.T) {
	ts := newTestServer(t)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/collections", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-NanoDB-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("X-NanoDB-Token 鉴权 = %d", resp.StatusCode)
	}
}

func TestHTTPTransactions(t *testing.T) {
	ts := newTestServer(t)

	begin := func() string {
		t.Helper()
		status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions", testToken, "")
		if status != http.StatusCreated {
			t.Fatalf("开启事务 = %d, body=%s", status, body)
		}
		id, _ := decode[map[string]any](t, body)["tx_id"].(string)
		if id == "" {
			t.Fatalf("缺少 tx_id: %s", body)
		}
		return id
	}

	txID := begin()
	status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+txID+"/documents/users", testToken,
		`{"name":"Alice","age":30}`)
	if status != http.StatusCreated {
		t.Fatalf("事务内插入 = %d, body=%s", status, body)
	}
	docID, _ := decode[map[string]any](t, body)["_id"].(string)

	// 提交前对普通接口不可见。
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users/"+docID, testToken, ""); status != http.StatusNotFound {
		t.Fatalf("提交前读取 = %d, want 404", status)
	}
	// 事务内可见并能继续更新。
	status, body = doRequest(t, http.MethodPatch, ts.URL+"/v1/transactions/"+txID+"/documents/users/"+docID, testToken,
		`{"$inc":{"age":1}}`)
	if status != http.StatusOK {
		t.Fatalf("事务内更新 = %d, body=%s", status, body)
	}
	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/transactions/"+txID+"/documents/users?count=1", testToken, "")
	if status != http.StatusOK || decode[map[string]any](t, body)["count"].(float64) != 1 {
		t.Fatalf("事务内计数 = %d, body=%s", status, body)
	}
	status, body = doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+txID+"/commit", testToken, "")
	if status != http.StatusOK {
		t.Fatalf("提交 = %d, body=%s", status, body)
	}
	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/users/"+docID, testToken, "")
	if status != http.StatusOK || decode[map[string]any](t, body)["age"].(float64) != 31 {
		t.Fatalf("提交后读取 = %d, body=%s", status, body)
	}

	// 回滚丢弃事务内写入。
	rollbackID := begin()
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+rollbackID+"/ops", testToken,
		`[{"op":"insert","coll":"users","doc":{"_id":"bob","name":"Bob"}}]`); status != http.StatusOK {
		t.Fatalf("事务内批量 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+rollbackID+"/rollback", testToken, ""); status != http.StatusOK {
		t.Fatalf("回滚 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users/bob", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("回滚后读取 = %d, want 404", status)
	}

	// 事务期间外部写入导致提交冲突。
	conflictID := begin()
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/transactions/"+conflictID+"/documents/users", testToken, ""); status != http.StatusOK {
		t.Fatalf("事务内查询 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+conflictID+"/documents/users", testToken,
		`{"_id":"cara","name":"Cara"}`); status != http.StatusCreated {
		t.Fatalf("事务内插入 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, `{"_id":"outside","name":"Outside"}`); status != http.StatusCreated {
		t.Fatalf("外部插入 = %d", status)
	}
	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+conflictID+"/commit", testToken, ""); status != http.StatusConflict {
		t.Fatalf("冲突提交 = %d, want 409, body=%s", status, body)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users/cara", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("冲突回滚后读取 = %d, want 404", status)
	}

	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/not-exist/commit", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("未知事务 = %d, want 404", status)
	}
}

func TestHTTPIndexesAndBatch(t *testing.T) {
	ts := newTestServer(t)

	status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/users", testToken, `{"field":"age"}`)
	if status != http.StatusCreated {
		t.Fatalf("创建索引 = %d, body=%s", status, body)
	}
	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/indexes/users", testToken, "")
	if status != http.StatusOK {
		t.Fatalf("索引列表 = %d, body=%s", status, body)
	}
	if specs := decode[[]engine.IndexSpec](t, body); len(specs) != 1 || specs[0].Field != "age" {
		t.Fatalf("索引列表错误: %+v", specs)
	}

	status, body = doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken,
		`[{"_id":"a","age":30},{"_id":"b","age":20}]`)
	if status != http.StatusCreated {
		t.Fatalf("批量插入 = %d, body=%s", status, body)
	}

	batch := `[
		{"op":"insert","coll":"users","doc":{"_id":"c","age":40}},
		{"op":"update","coll":"users","id":"a","patch":{"$inc":{"age":1}}}
	]`
	status, body = doRequest(t, http.MethodPost, ts.URL+"/v1/batch", testToken, batch)
	if status != http.StatusOK {
		t.Fatalf("原子批量 = %d, body=%s", status, body)
	}
	resp := decode[map[string]any](t, body)
	if resp["count"].(float64) != 2 {
		t.Fatalf("批量结果数量 = %v", resp["count"])
	}

	filter := url.QueryEscape(`{"age":{"$gte":31}}`)
	status, body = doRequest(t, http.MethodGet, ts.URL+"/v1/users?filter="+filter+"&sort=age:-1", testToken, "")
	if status != http.StatusOK {
		t.Fatalf("索引查询 = %d, body=%s", status, body)
	}
	docs := decode[[]map[string]any](t, body)
	if len(docs) != 2 || docs[0]["_id"] != "c" || docs[1]["_id"] != "a" {
		t.Fatalf("索引查询结果错误: %v", docs)
	}

	failing := `[
		{"op":"insert","coll":"users","doc":{"_id":"z","age":1}},
		{"op":"update","coll":"users","id":"missing","patch":{"$set":{"age":2}}}
	]`
	status, _ = doRequest(t, http.MethodPost, ts.URL+"/v1/batch", testToken, failing)
	if status != http.StatusNotFound {
		t.Fatalf("失败批量 = %d, want 404", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users/z", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("失败批量中的插入不应生效, 状态 = %d", status)
	}

	status, body = doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/users", testToken, `{"field":"email","unique":true}`)
	if status != http.StatusCreated {
		t.Fatalf("创建唯一索引 = %d, body=%s", status, body)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, `{"_id":"d","email":"d@example.com"}`); status != http.StatusCreated {
		t.Fatalf("唯一索引插入 = %d", status)
	}
	dupBatch := `[{"op":"insert","coll":"users","doc":{"_id":"e","email":"d@example.com"}}]`
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/batch", testToken, dupBatch); status != http.StatusConflict {
		t.Fatalf("批量唯一冲突 = %d, want 409", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users/e", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("唯一冲突时插入不应生效, 状态 = %d", status)
	}

	if status, _ := doRequest(t, http.MethodDelete, ts.URL+"/v1/indexes/users/age", testToken, ""); status != http.StatusOK {
		t.Fatalf("删除索引 = %d", status)
	}
	if status, body := doRequest(t, http.MethodGet, ts.URL+"/v1/indexes/users", testToken, ""); status != http.StatusOK {
		t.Fatalf("索引列表 = %d, body=%s", status, body)
	} else if specs := decode[[]engine.IndexSpec](t, body); len(specs) != 1 || specs[0].Field != "email" {
		t.Fatalf("删除索引后列表错误: %+v", specs)
	}

	badBatch := `[{"op":"upsert","coll":"users","doc":{}}]`
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/batch", testToken, badBatch); status != http.StatusBadRequest {
		t.Fatalf("非法批量操作 = %d, want 400", status)
	}
}

func TestHTTPBodyValidation(t *testing.T) {
	ts := newTestServer(t)

	huge := `{"name":"` + strings.Repeat("a", 9<<20) + `"}`
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, huge); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体 = %d, want 413", status)
	}

	trailing := `{"name":"A"}{"name":"B"}`
	if status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, trailing); status != http.StatusBadRequest {
		t.Fatalf("尾部多余 JSON = %d, body=%s, want 400", status, body)
	}

	if status, body := doRequest(t, http.MethodGet, ts.URL+"/v1/users", testToken, ""); status != http.StatusOK {
		t.Fatalf("查询 = %d, body=%s", status, body)
	} else if docs := decode[[]map[string]any](t, body); len(docs) != 0 {
		t.Fatalf("非法请求不应写入文档: %v", docs)
	}
}
