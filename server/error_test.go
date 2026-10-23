package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nanodb/engine"
	"nanodb/server"
)

func doRequestHeader(t *testing.T, method, rawURL, token, body, header string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get(header)
}

func TestHTTPAuthAndRoutingErrors(t *testing.T) {
	ts := newTestServer(t)

	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/healthz", "", ""); status != http.StatusOK {
		t.Fatalf("healthz 无需鉴权，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("缺少 token 应返回 401，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/", "wrong", ""); status != http.StatusUnauthorized {
		t.Fatalf("错误 token 应返回 401，得到 %d", status)
	}

	for _, path := range []string{"/nope", "/v1", "/v1/", "/v1/admin/unknown"} {
		if status, _ := doRequest(t, http.MethodGet, ts.URL+path, testToken, ""); status != http.StatusNotFound {
			t.Fatalf("GET %s 应返回 404，得到 %d", path, status)
		}
	}

	allowed := []struct {
		method, path, want string
	}{
		{http.MethodPatch, "/v1/collections", "GET, DELETE"},
		{http.MethodGet, "/v1/batch", "POST"},
		{http.MethodPost, "/v1/admin/stats", "GET"},
		{http.MethodPut, "/v1/transactions", "POST"},
	}
	for _, c := range allowed {
		status, allow := doRequestHeader(t, c.method, ts.URL+c.path, testToken, "", "Allow")
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s 应返回 405，得到 %d", c.method, c.path, status)
		}
		if allow != c.want {
			t.Fatalf("%s %s 的 Allow = %q, want %q", c.method, c.path, allow, c.want)
		}
	}
}

func TestHTTPCollectionErrors(t *testing.T) {
	ts := newTestServer(t)

	for _, body := range []string{`{`, `123`, `[{"a":1},2]`} {
		if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/err", testToken, body); status != http.StatusBadRequest {
			t.Fatalf("POST body %q 应返回 400，得到 %d", body, status)
		}
	}

	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/dup", testToken, `{"_id":"x"}`); status != http.StatusCreated {
		t.Fatalf("首次插入应返回 201，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/dup", testToken, `{"_id":"x"}`); status != http.StatusConflict {
		t.Fatalf("重复 _id 应返回 409，得到 %d", status)
	}

	queries := []string{
		"?filter=%7B",
		"?sort=age:up",
		"?limit=abc",
		"?limit=-1",
		"?skip=-1",
	}
	for _, q := range queries {
		if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/dup"+q, testToken, ""); status != http.StatusBadRequest {
			t.Fatalf("GET %s 应返回 400，得到 %d", q, status)
		}
	}
	if status, _ := doRequest(t, http.MethodDelete, ts.URL+"/v1/dup?filter=%7B", testToken, ""); status != http.StatusBadRequest {
		t.Fatalf("非法 filter 的 DELETE 应返回 400，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/dup/x/y", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("多余路径段应返回 404，得到 %d", status)
	}
}

func TestHTTPDocumentErrors(t *testing.T) {
	ts := newTestServer(t)

	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users/missing", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("读取不存在的文档应返回 404，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPut, ts.URL+"/v1/users/missing", testToken, `{"name":"x"}`); status != http.StatusNotFound {
		t.Fatalf("替换不存在的文档应返回 404，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPatch, ts.URL+"/v1/users/missing", testToken, `{"$set":{"a":1}}`); status != http.StatusNotFound {
		t.Fatalf("更新不存在的文档应返回 404，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodDelete, ts.URL+"/v1/users/missing", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("删除不存在的文档应返回 404，得到 %d", status)
	}

	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, `{"_id":"d1","age":1}`); status != http.StatusCreated {
		t.Fatalf("插入应返回 201，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPatch, ts.URL+"/v1/users/d1", testToken, `{"$inc":{"age":"x"}}`); status != http.StatusBadRequest {
		t.Fatalf("非法更新表达式应返回 400，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPut, ts.URL+"/v1/users/d1", testToken, `[1]`); status != http.StatusBadRequest {
		t.Fatalf("替换 body 不是对象应返回 400，得到 %d", status)
	}
	if status, allow := doRequestHeader(t, http.MethodPost, ts.URL+"/v1/users/d1", testToken, "", "Allow"); status != http.StatusMethodNotAllowed {
		t.Fatalf("文档路径 POST 应返回 405，得到 %d", status)
	} else if allow != "GET, PUT, PATCH, DELETE" {
		t.Fatalf("Allow = %q", allow)
	}
}

func TestHTTPIndexErrors(t *testing.T) {
	ts := newTestServer(t)

	for _, body := range []string{`{`, `{}`} {
		if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/users", testToken, body); status != http.StatusBadRequest {
			t.Fatalf("创建索引 body %q 应返回 400，得到 %d", body, status)
		}
	}
	// 删除不存在的索引是幂等操作。
	if status, _ := doRequest(t, http.MethodDelete, ts.URL+"/v1/indexes/users/missing", testToken, ""); status != http.StatusOK {
		t.Fatalf("删除不存在的索引应返回 200，得到 %d", status)
	}

	for _, id := range []string{"a", "b"} {
		if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/uniq", testToken, `{"_id":"`+id+`","email":"x"}`); status != http.StatusCreated {
			t.Fatalf("插入 %s 应返回 201，得到 %d", id, status)
		}
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/uniq", testToken, `{"field":"email","unique":true}`); status != http.StatusConflict {
		t.Fatalf("重复值上建唯一索引应返回 409，得到 %d", status)
	}
}

func TestHTTPBatchAndTxErrors(t *testing.T) {
	ts := newTestServer(t)

	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/batch", testToken, `[]`); status != http.StatusBadRequest {
		t.Fatalf("空 batch 应返回 400，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/batch", testToken, `[{"op":"nope","coll":"c"}]`); status != http.StatusBadRequest {
		t.Fatalf("非法 op 应返回 400，得到 %d", status)
	}

	txPaths := []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/transactions/nope/commit"},
		{http.MethodPost, "/v1/transactions/nope/rollback"},
		{http.MethodDelete, "/v1/transactions/nope"},
		{http.MethodPost, "/v1/transactions/nope/ops"},
		{http.MethodGet, "/v1/transactions/nope/documents/users"},
	}
	for _, c := range txPaths {
		if status, _ := doRequest(t, c.method, ts.URL+c.path, testToken, `[]`); status != http.StatusNotFound {
			t.Fatalf("%s %s 应返回 404，得到 %d", c.method, c.path, status)
		}
	}

	status, data := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions", testToken, "")
	if status != http.StatusCreated {
		t.Fatalf("开启事务应返回 201，得到 %d", status)
	}
	var created struct {
		TxID string `json:"tx_id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.TxID == "" {
		t.Fatalf("解析 tx_id 失败: %v, body=%s", err, data)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+created.TxID+"/ops", testToken, `[]`); status != http.StatusBadRequest {
		t.Fatalf("空事务 ops 应返回 400，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+created.TxID+"/commit", testToken, ""); status != http.StatusOK {
		t.Fatalf("提交应返回 200，得到 %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+created.TxID+"/commit", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("重复提交应返回 404，得到 %d", status)
	}
}

func TestServerCloseRollsBackTransactions(t *testing.T) {
	db, err := engine.Open(t.TempDir(), &engine.Options{Sync: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	api := server.New(db, testToken)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	status, data := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions", testToken, "")
	if status != http.StatusCreated {
		t.Fatalf("开启事务应返回 201，得到 %d", status)
	}
	var created struct {
		TxID string `json:"tx_id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.TxID == "" {
		t.Fatalf("解析 tx_id 失败: %v, body=%s", err, data)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+created.TxID+"/ops", testToken,
		`[{"op":"insert","coll":"users","doc":{"_id":"a"}}]`); status != http.StatusOK {
		t.Fatalf("事务写入应返回 200，得到 %d", status)
	}

	api.Close()

	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/transactions/"+created.TxID+"/commit", testToken, ""); status != http.StatusNotFound {
		t.Fatalf("Close 后事务应被回收，commit 得到 %d", status)
	}
	if _, ok := db.Get("users", "a"); ok {
		t.Fatal("Close 应回滚未提交的事务")
	}
}

func TestHTTPClosedDatabase(t *testing.T) {
	db, err := engine.Open(t.TempDir(), &engine.Options{Sync: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ts := httptest.NewServer(server.New(db, testToken).Handler())
	t.Cleanup(ts.Close)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/users", testToken, `{"name":"x"}`); status != http.StatusServiceUnavailable {
		t.Fatalf("数据库关闭后写入应返回 503，得到 %d", status)
	}
}
