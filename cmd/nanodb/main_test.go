package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"nanodb/engine"
)

// runCapture 执行一次 CLI 调用并捕获标准输出。
func runCapture(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	outCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		outCh <- string(data)
	}()
	os.Stdout = w
	runErr := run(append([]string{"-db", dir, "-no-sync"}, args...))
	_ = w.Close()
	os.Stdout = old
	return <-outCh, runErr
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{`put users '{"name":"Alice"}'`, []string{"put", "users", `{"name":"Alice"}`}},
		{`find users -sort age:-1 -limit 10`, []string{"find", "users", "-sort", "age:-1", "-limit", "10"}},
		{`  a   "b c"  ''  `, []string{"a", "b c", ""}},
		{``, nil},
	}
	for _, c := range cases {
		if got := splitArgs(c.line); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("splitArgs(%q) = %#v, want %#v", c.line, got, c.want)
		}
	}
}

func TestParseFindArgs(t *testing.T) {
	coll, q, err := parseFindArgs([]string{
		"users", `{"age":{"$gte":18}}`,
		"-sort", "age:-1", "-limit", "10", "-skip", "5", "-fields", "name, age",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if coll != "users" {
		t.Fatalf("collection = %q, want users", coll)
	}
	if len(q.Sort) != 1 || q.Sort[0].Field != "age" || !q.Sort[0].Desc {
		t.Fatalf("sort = %#v", q.Sort)
	}
	if q.Limit != 10 || q.Skip != 5 {
		t.Fatalf("limit/skip = %d/%d, want 10/5", q.Limit, q.Skip)
	}
	if !reflect.DeepEqual(q.Fields, []string{"name", "age"}) {
		t.Fatalf("fields = %#v", q.Fields)
	}
	if q.Filter["age"] == nil {
		t.Fatalf("filter = %#v", q.Filter)
	}
}

func TestParseFindArgsErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"users", "-limit"},
		{"users", "-limit", "abc"},
		{"users", "-limit", "-1"},
		{"users", "-skip", "x"},
		{"users", "-sort", "age:up"},
		{"users", "-unknown"},
		{"users", "{}", "extra"},
		{"users", "not-json"},
	}
	for _, args := range cases {
		if _, _, err := parseFindArgs(args); err == nil {
			t.Fatalf("parseFindArgs(%#v) 应返回错误", args)
		}
	}
}

func TestParseObject(t *testing.T) {
	doc, err := parseObject(`{"name":"Alice","age":30}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc["name"] != "Alice" {
		t.Fatalf("doc = %#v", doc)
	}
	for _, s := range []string{`null`, `[]`, `{`, ``} {
		if _, err := parseObject(s); err == nil {
			t.Fatalf("parseObject(%q) 应返回错误", s)
		}
	}
}

func TestDisplayAddr(t *testing.T) {
	cases := map[string]string{
		":8080":           "localhost:8080",
		"127.0.0.1:9000":  "127.0.0.1:9000",
		"example.com:443": "example.com:443",
		"[::1]:8080":      "[::1]:8080",
	}
	for in, want := range cases {
		if got := displayAddr(in); got != want {
			t.Fatalf("displayAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDataCommand(t *testing.T) {
	for _, cmd := range []string{"put", "get", "find", "count", "update", "delete", "batch"} {
		if !isDataCommand(cmd) {
			t.Fatalf("%q 应为数据命令", cmd)
		}
	}
	for _, cmd := range []string{"index", "collections", "stats", "compact", "drop"} {
		if isDataCommand(cmd) {
			t.Fatalf("%q 不应为数据命令", cmd)
		}
	}
}

func TestRunCLIWorkflow(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"put", "users", `{"_id":"u1","name":"Alice","age":30}`}, "Alice"},
		{[]string{"get", "users", "u1"}, "Alice"},
		{[]string{"count", "users"}, `"count": 1`},
		{[]string{"find", "users", `{"age":{"$gte":18}}`, "-sort", "age:-1", "-limit", "1"}, "Alice"},
		{[]string{"update", "users", "u1", `{"$inc":{"age":1}}`}, `"age": 31`},
		{[]string{"index", "create", "users", "age"}, `"created": true`},
		{[]string{"index", "list", "users"}, "age"},
		{[]string{"stats"}, "collections"},
		{[]string{"compact"}, `"compacted": true`},
		{[]string{"collections"}, "users"},
		{[]string{"batch", `[{"op":"insert","coll":"users","doc":{"_id":"u2","name":"Bob"}}]`}, "Bob"},
		{[]string{"delete", "users", "u1"}, `"deleted": true`},
		{[]string{"drop", "users"}, `"dropped": "users"`},
	}
	for _, c := range cases {
		out, err := runCapture(t, dir, c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !strings.Contains(out, c.want) {
			t.Fatalf("%v 输出缺少 %q:\n%s", c.args, c.want, out)
		}
	}
}

func TestRunCLIErrors(t *testing.T) {
	dir := t.TempDir()
	cases := [][]string{
		{"put", "users", `{`},
		{"get"},
		{"get", "users", "nope"},
		{"delete", "users", "nope"},
		{"find", "users", "not-json"},
		{"find", "users", "{}", "extra"},
		{"update", "users", "x"},
		{"index", "nope", "users", "age"},
		{"batch", "[]"},
		{"batch", "not-json"},
		{"unknown-command"},
	}
	for _, args := range cases {
		if _, err := runCapture(t, dir, args...); err == nil {
			t.Fatalf("%v 应返回错误", args)
		}
	}
}

func TestRunHelp(t *testing.T) {
	out, err := runCapture(t, t.TempDir(), "help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out, "用法") {
		t.Fatalf("help 输出异常:\n%s", out)
	}
}

func openShellDB(t *testing.T) *engine.DB {
	t.Helper()
	db, err := engine.Open(t.TempDir(), &engine.Options{Sync: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunShellSession(t *testing.T) {
	db := openShellDB(t)
	in := strings.NewReader(strings.Join([]string{
		`put users '{"_id":"u1","name":"Alice"}'`,
		`begin`,
		`put users '{"_id":"u2","name":"Bob"}'`,
		`commit`,
		`count users`,
		`help`,
		`serve`,
		`exit`,
	}, "\n"))
	var out, errOut bytes.Buffer
	if err := runShellIO(db, in, &out, &errOut); err != nil {
		t.Fatalf("shell: %v", err)
	}
	n, err := db.Count("users", nil)
	if err != nil || n != 2 {
		t.Fatalf("事务提交后应有 2 条文档，得到 %d, err=%v", n, err)
	}
	for _, want := range []string{"nano> ", "nano(tx)> ", "事务已开始", "事务已提交", `"count": 2`, "可用命令", "请退出 shell"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("shell 输出缺少 %q:\n%s", want, out.String())
		}
	}
	if errOut.Len() != 0 {
		t.Fatalf("不应有错误输出: %s", errOut.String())
	}
}

func TestRunShellRollbackOnEOF(t *testing.T) {
	db := openShellDB(t)
	in := strings.NewReader("begin\nput users '{\"_id\":\"u1\",\"name\":\"Alice\"}'\n")
	var out, errOut bytes.Buffer
	if err := runShellIO(db, in, &out, &errOut); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !strings.Contains(out.String(), "未提交的事务已回滚") {
		t.Fatalf("EOF 应回滚事务:\n%s", out.String())
	}
	if n, err := db.Count("users", nil); err != nil || n != 0 {
		t.Fatalf("回滚后不应有文档，得到 %d, err=%v", n, err)
	}
}

func TestRunShellErrors(t *testing.T) {
	db := openShellDB(t)
	in := strings.NewReader("commit\nget users missing\nbogus\nexit\n")
	var out, errOut bytes.Buffer
	if err := runShellIO(db, in, &out, &errOut); err != nil {
		t.Fatalf("shell: %v", err)
	}
	for _, want := range []string{"没有进行中的事务", "文档不存在", "未知命令"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("错误输出缺少 %q:\n%s", want, errOut.String())
		}
	}
}

func TestServeHTTPStartupAndShutdown(t *testing.T) {
	db := openShellDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, db, "127.0.0.1:0", "secret", func(string, ...any) {}, func(addr string) {
			ready <- addr
		})
	}()

	var addr string
	select {
	case addr = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("服务未在 5s 内启动")
	}

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz 应返回 200，得到 %d", resp.StatusCode)
	}

	resp, err = client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("设置 token 后根路径应返回 401，得到 %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("关闭服务出错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("服务未在 5s 内停止")
	}
}

func TestServeHTTPListenError(t *testing.T) {
	db := openShellDB(t)
	if err := serveHTTP(context.Background(), db, "127.0.0.1:-1", "", nil, nil); err == nil {
		t.Fatal("非法监听地址应返回错误")
	}
}
