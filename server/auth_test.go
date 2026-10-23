package server_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"nanodb/engine"
	"nanodb/server"
)

func TestUserStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store, err := server.OpenUserStore(path)
	if err != nil {
		t.Fatalf("OpenUserStore: %v", err)
	}
	alice, err := store.Add("alice", server.RoleRead, []string{"users"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(alice.Token) != 64 {
		t.Fatalf("token 长度 = %d, want 64", len(alice.Token))
	}
	if _, err := store.Add("alice", server.RoleWrite, nil); !errors.Is(err, server.ErrUserExists) {
		t.Fatalf("重复用户名期望 ErrUserExists，得到 %v", err)
	}
	if _, err := store.Add("bob", "root", nil); !errors.Is(err, server.ErrInvalidRole) {
		t.Fatalf("非法角色期望 ErrInvalidRole，得到 %v", err)
	}
	if got, ok := store.Lookup(alice.Token); !ok || got.Name != "alice" || got.Role != server.RoleRead {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
	if _, ok := store.Lookup("nope"); ok {
		t.Fatal("未知 token 不应命中")
	}

	// 新实例读取同一文件，验证持久化。
	store2, err := server.OpenUserStore(path)
	if err != nil {
		t.Fatalf("OpenUserStore 二次打开: %v", err)
	}
	got, ok := store2.Get("alice")
	if !ok || len(got.Collections) != 1 || got.Collections[0] != "users" {
		t.Fatalf("持久化后用户 = %+v, %v", got, ok)
	}

	if err := store.Remove("alice"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := store.Remove("alice"); !errors.Is(err, server.ErrUserNotFound) {
		t.Fatalf("重复删除期望 ErrUserNotFound，得到 %v", err)
	}
}

func TestHTTPRolesAndCollectionACL(t *testing.T) {
	dir := t.TempDir()
	db, err := engine.Open(dir, &engine.Options{Sync: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	store, err := server.OpenUserStore(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("OpenUserStore: %v", err)
	}
	reader, err := store.Add("reader", server.RoleRead, nil)
	if err != nil {
		t.Fatalf("Add reader: %v", err)
	}
	writer, err := store.Add("writer", server.RoleWrite, []string{"users"})
	if err != nil {
		t.Fatalf("Add writer: %v", err)
	}
	admin, err := store.Add("root", server.RoleAdmin, nil)
	if err != nil {
		t.Fatalf("Add admin: %v", err)
	}

	ts := httptest.NewServer(server.NewWithOptions(db, server.Options{
		Token: "root-token",
		Users: store,
	}).Handler())
	defer ts.Close()

	// 未携带或错误的凭据。
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("无 token = %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users", "bad", ""); status != http.StatusUnauthorized {
		t.Fatalf("错误 token = %d", status)
	}
	// 健康检查保持公开。
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/healthz", "", ""); status != http.StatusOK {
		t.Fatalf("healthz = %d", status)
	}

	// read 角色：可读、不可写、不可进管理接口。
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users", reader.Token, ""); status != http.StatusOK {
		t.Fatalf("read GET = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/users", reader.Token, `{"_id":"a"}`); status != http.StatusForbidden {
		t.Fatalf("read POST = %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/admin/stats", reader.Token, ""); status != http.StatusForbidden {
		t.Fatalf("read admin = %d", status)
	}

	// write 角色：集合白名单内可写，白名单外读写都拒绝。
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/users", writer.Token, `{"_id":"b","name":"x"}`); status != http.StatusCreated {
		t.Fatalf("write 白名单内 POST = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/other", writer.Token, `{"_id":"c"}`); status != http.StatusForbidden {
		t.Fatalf("write 白名单外 POST = %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/other", writer.Token, ""); status != http.StatusForbidden {
		t.Fatalf("write 白名单外 GET = %d", status)
	}
	batch := `[{"op":"insert","coll":"other","doc":{"_id":"x"}}]`
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/batch", writer.Token, batch); status != http.StatusForbidden {
		t.Fatalf("write 批量越权 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/aggregate/other", writer.Token, `{"pipeline":[]}`); status != http.StatusForbidden {
		t.Fatalf("write 聚合越权 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/aggregate/users", writer.Token, `{"pipeline":[{"$count":"n"}]}`); status != http.StatusOK {
		t.Fatalf("write 聚合 = %d", status)
	}

	// 索引 DDL 与删除集合只允许 admin。
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/users", writer.Token, `{"field":"age"}`); status != http.StatusForbidden {
		t.Fatalf("write 建索引 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/indexes/users", admin.Token, `{"field":"age"}`); status != http.StatusCreated {
		t.Fatalf("admin 建索引 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodDelete, ts.URL+"/v1/collections/users", writer.Token, ""); status != http.StatusForbidden {
		t.Fatalf("write 删集合 = %d", status)
	}

	// 共享 token 等价 admin。
	if status, _ := doRequest(t, http.MethodPost, ts.URL+"/v1/admin/compact", "root-token", ""); status != http.StatusOK {
		t.Fatalf("共享 token compact = %d", status)
	}

	// admin 通过 HTTP 新建用户后，可立即用其 token 访问。
	status, body := doRequest(t, http.MethodPost, ts.URL+"/v1/admin/users", admin.Token, `{"name":"newbie","role":"read"}`)
	if status != http.StatusCreated {
		t.Fatalf("创建用户 = %d, %s", status, body)
	}
	var created server.User
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("解析用户: %v", err)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users", created.Token, ""); status != http.StatusOK {
		t.Fatalf("新用户 GET = %d", status)
	}
	if status, _ := doRequest(t, http.MethodDelete, ts.URL+"/v1/admin/users/newbie", admin.Token, ""); status != http.StatusOK {
		t.Fatalf("删除用户 = %d", status)
	}
	if status, _ := doRequest(t, http.MethodGet, ts.URL+"/v1/users", created.Token, ""); status != http.StatusUnauthorized {
		t.Fatalf("删除后 token = %d", status)
	}
}
