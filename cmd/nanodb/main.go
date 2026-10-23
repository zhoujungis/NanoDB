package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"nanodb/engine"
	"nanodb/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("nanodb", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath := fs.String("db", "./nanodb-data", "数据库目录")
	noSync := fs.Bool("no-sync", false, "关闭每次写入的 fsync（更快，但掉电可能丢失最近写入）")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	rest := fs.Args()
	cmd := "shell"
	if len(rest) > 0 {
		cmd = rest[0]
		rest = rest[1:]
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		return nil
	}
	// 用户管理只改 users.json，不打开数据库，避免与运行中的服务争抢目录锁。
	if cmd == "user" {
		return runUserCommand(filepath.Join(*dbPath, "users.json"), rest, os.Stdout)
	}

	db, err := engine.Open(*dbPath, &engine.Options{Sync: !*noSync})
	if err != nil {
		return err
	}
	defer db.Close()

	if cmd == "serve" {
		return serve(db, rest)
	}
	return dispatch(db, db, os.Stdin, os.Stdout, os.Stderr, cmd, rest)
}

func usage() {
	fmt.Print(`NanoDB ` + engine.Version + ` — 轻量级嵌入式文档数据库

用法:
  nanodb [-db 目录] [-no-sync] <命令> [参数...]

命令:
  shell                          进入交互式 shell（默认），支持 begin/commit/rollback 事务
  put <集合> <JSON文档>           插入文档，未提供 _id 时自动生成
  get <集合> <id>                 读取单个文档
  find <集合> [过滤JSON]          查询文档
       -sort 字段:-1,...  -limit N  -skip N  -fields 字段1,字段2
  count <集合> [过滤JSON]         统计文档数量
  aggregate <集合> <管道JSON|->   执行聚合管道，支持 $match/$group/$sort/$skip/$limit/$project/$unwind/$count
  sql <语句|->                    执行只读 SQL（SELECT/WHERE/ORDER BY/LIMIT/OFFSET/COUNT(*)）
  update <集合> <id> <更新JSON>   更新文档，支持 $set/$unset/$inc/$push/$pull/$addToSet
  delete <集合> <id>              删除单个文档
  drop <集合>                     删除整个集合
  index create <集合> <字段[,字段2...]> [unique]  创建二级索引；逗号分隔为复合索引
  index create <集合> * text      创建全文索引（覆盖所有字符串字段，查询用 $text）
  index create <集合> <字段> geo   创建地理索引（查询用 $geoWithin）
  index drop <集合> <字段[,字段2...]>    删除索引
  index list <集合>               查看索引
  batch <操作JSON数组|->          原子批量写入；- 表示从标准输入读取
  collections                     列出所有集合
  stats                           查看数据库统计
  compact                         生成快照并清空 WAL
  serve [-addr :8080] [-token X]  启动 HTTP 服务；token 为共享 admin 凭据
  user add <名字> [角色] [集合,...] 新建账号（角色 admin/write/read，默认 write）
  user list                       列出账号及其 token
  user remove <名字>              删除账号

示例:
  nanodb -db ./data put users '{"name":"Alice","age":30}'
  nanodb -db ./data find users '{"age":{"$gte":18}}' -sort age:-1 -limit 10
  nanodb -db ./data update users <id> '{"$inc":{"age":1}}'
  nanodb -db ./data index create users age
  nanodb -db ./data index create users * text
  nanodb -db ./data index create users location geo
  nanodb -db ./data sql 'SELECT name, age FROM users WHERE age >= 18 ORDER BY age DESC LIMIT 10'
  nanodb -db ./data batch '[{"op":"insert","coll":"users","doc":{"name":"A"}},{"op":"insert","coll":"users","doc":{"name":"B"}}]'
  nanodb -db ./data serve -addr :8080 -token secret

交互式事务:
  nano> begin        开启事务，提示符变为 nano(tx)>
  nano(tx)> put ...  事务内的读写基于快照，提交前其他会话不可见
  nano(tx)> commit   原子提交；冲突时返回错误并丢弃全部改动
  nano(tx)> rollback 放弃全部改动
`)
}

// dataStore 是 DB 与 Tx 共有的数据读写能力。
type dataStore interface {
	Insert(coll string, doc map[string]any) (map[string]any, error)
	Get(coll, id string) (map[string]any, bool)
	Find(coll string, q engine.Query) ([]map[string]any, error)
	Count(coll string, filter map[string]any) (int, error)
	Update(coll, id string, patch map[string]any) (map[string]any, error)
	Delete(coll, id string) (bool, error)
	Batch(ops []engine.BatchOp) ([]engine.BatchResult, error)
}

func isDataCommand(cmd string) bool {
	switch cmd {
	case "put", "get", "find", "count", "update", "delete", "batch", "sql":
		return true
	default:
		return false
	}
}

// dispatch 执行命令；数据读写走 s（数据库或事务），管理类命令只走 db。
func dispatch(s dataStore, db *engine.DB, in io.Reader, out, errOut io.Writer, cmd string, args []string) error {
	if _, isTx := s.(*engine.Tx); isTx && !isDataCommand(cmd) {
		return fmt.Errorf("事务中不支持命令 %q，请先 commit 或 rollback", cmd)
	}
	switch cmd {
	case "shell":
		return runShellIO(db, in, out, errOut)

	case "put":
		if len(args) < 2 {
			return errors.New(`用法: put <集合> '{"字段":值}'`)
		}
		doc, err := parseObject(args[1])
		if err != nil {
			return err
		}
		doc, err = s.Insert(args[0], doc)
		if err != nil {
			return err
		}
		return printJSON(out, doc)

	case "get":
		if len(args) < 2 {
			return errors.New("用法: get <集合> <id>")
		}
		doc, ok := s.Get(args[0], args[1])
		if !ok {
			return fmt.Errorf("文档不存在: %s/%s", args[0], args[1])
		}
		return printJSON(out, doc)

	case "find":
		coll, q, err := parseFindArgs(args)
		if err != nil {
			return err
		}
		docs, err := s.Find(coll, q)
		if err != nil {
			return err
		}
		return printJSON(out, docs)

	case "count":
		if len(args) < 1 {
			return errors.New("用法: count <集合> [过滤JSON]")
		}
		var filter map[string]any
		if len(args) > 1 {
			var err error
			filter, err = parseObject(args[1])
			if err != nil {
				return err
			}
		}
		n, err := s.Count(args[0], filter)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"count": n})

	case "sql":
		if len(args) < 1 {
			return errors.New("用法: sql 'SELECT ... FROM 集合 [WHERE ...] [ORDER BY ...] [LIMIT N]'，用 - 从标准输入读取")
		}
		var stmt string
		if args[0] == "-" {
			data, err := io.ReadAll(in)
			if err != nil {
				return fmt.Errorf("读取标准输入失败: %w", err)
			}
			stmt = string(data)
		} else {
			stmt = strings.Join(args, " ")
		}
		docs, err := engine.RunSQL(s, stmt)
		if err != nil {
			return err
		}
		return printJSON(out, docs)

	case "update":
		if len(args) < 3 {
			return errors.New(`用法: update <集合> <id> '{"$set":{"字段":值}}'`)
		}
		patch, err := parseObject(args[2])
		if err != nil {
			return err
		}
		updated, err := s.Update(args[0], args[1], patch)
		if err != nil {
			if errors.Is(err, engine.ErrNotFound) {
				return fmt.Errorf("文档不存在: %s/%s", args[0], args[1])
			}
			return err
		}
		return printJSON(out, updated)

	case "delete":
		if len(args) < 2 {
			return errors.New("用法: delete <集合> <id>")
		}
		ok, err := s.Delete(args[0], args[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("文档不存在: %s/%s", args[0], args[1])
		}
		return printJSON(out, map[string]any{"deleted": true})

	case "drop":
		if len(args) < 1 {
			return errors.New("用法: drop <集合>")
		}
		if err := db.DropCollection(args[0]); err != nil {
			return err
		}
		return printJSON(out, map[string]any{"dropped": args[0]})

	case "index":
		if len(args) < 2 {
			return errors.New("用法: index create <集合> <字段> [unique] | index drop <集合> <字段> | index list <集合>")
		}
		switch args[0] {
		case "create":
			if len(args) < 3 {
				return errors.New("用法: index create <集合> <字段[,字段2...]|*> [unique|text|geo]")
			}
			option := ""
			if len(args) > 3 {
				option = strings.ToLower(args[3])
			}
			spec := engine.IndexSpec{Field: args[2]}
			switch option {
			case "":
			case engine.IndexTypeText, engine.IndexTypeGeo:
				spec.Type = option
			case "unique", "true", "1":
				spec.Unique = true
			case "false", "0":
			default:
				return fmt.Errorf("未知索引选项 %q（可用 unique、text、geo）", args[3])
			}
			if err := db.CreateIndexSpec(args[1], spec); err != nil {
				return err
			}
			field := args[2]
			if spec.Type == engine.IndexTypeText {
				field = "*"
			}
			result := map[string]any{"created": true, "field": field, "unique": spec.Unique}
			if spec.Type != "" {
				result["type"] = spec.Type
			}
			return printJSON(out, result)
		case "drop":
			if len(args) < 3 {
				return errors.New("用法: index drop <集合> <字段>")
			}
			if err := db.DropIndex(args[1], args[2]); err != nil {
				return err
			}
			return printJSON(out, map[string]any{"dropped": args[2]})
		case "list":
			return printJSON(out, db.ListIndexes(args[1]))
		default:
			return fmt.Errorf("未知 index 子命令 %q", args[0])
		}

	case "batch":
		if len(args) < 1 {
			return errors.New(`用法: batch '[{"op":"insert","coll":"集合","doc":{...}}]'，用 - 从标准输入读取`)
		}
		var data []byte
		if args[0] == "-" {
			var err error
			data, err = io.ReadAll(in)
			if err != nil {
				return fmt.Errorf("读取标准输入失败: %w", err)
			}
		} else {
			data = []byte(args[0])
		}
		var ops []engine.BatchOp
		if err := json.Unmarshal(data, &ops); err != nil {
			return fmt.Errorf("JSON 解析失败: %w", err)
		}
		if len(ops) == 0 {
			return errors.New("批量操作不能为空")
		}
		results, err := s.Batch(ops)
		if err != nil {
			return err
		}
		return printJSON(out, results)

	case "aggregate":
		if len(args) < 2 {
			return errors.New(`用法: aggregate <集合> '[{"$group":{"_id":"$字段"}}]'，用 - 从标准输入读取`)
		}
		var data []byte
		if args[1] == "-" {
			var err error
			data, err = io.ReadAll(in)
			if err != nil {
				return fmt.Errorf("读取标准输入失败: %w", err)
			}
		} else {
			data = []byte(args[1])
		}
		var pipeline []map[string]any
		if err := json.Unmarshal(data, &pipeline); err != nil {
			return fmt.Errorf("JSON 解析失败: %w", err)
		}
		docs, err := db.Aggregate(args[0], pipeline)
		if err != nil {
			return err
		}
		return printJSON(out, docs)

	case "collections":
		return printJSON(out, db.Collections())

	case "stats":
		return printJSON(out, db.Stats())

	case "compact":
		if err := db.Compact(); err != nil {
			return err
		}
		return printJSON(out, map[string]any{"compacted": true})

	default:
		return fmt.Errorf("未知命令 %q，运行 nanodb help 查看帮助", cmd)
	}
}

func parseFindArgs(args []string) (string, engine.Query, error) {
	var (
		q   engine.Query
		pos []string
	)
	for i := 0; i < len(args); i++ {
		flagName := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s 需要一个参数", flagName)
			}
			i++
			return args[i], nil
		}
		switch flagName {
		case "-sort", "--sort":
			v, err := next()
			if err != nil {
				return "", q, err
			}
			sort, err := engine.ParseSort(v)
			if err != nil {
				return "", q, err
			}
			q.Sort = sort
		case "-limit", "--limit":
			v, err := next()
			if err != nil {
				return "", q, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return "", q, fmt.Errorf("limit 必须是非负整数: %q", v)
			}
			q.Limit = n
		case "-skip", "--skip":
			v, err := next()
			if err != nil {
				return "", q, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return "", q, fmt.Errorf("skip 必须是非负整数: %q", v)
			}
			q.Skip = n
		case "-fields", "--fields":
			v, err := next()
			if err != nil {
				return "", q, err
			}
			for _, f := range strings.Split(v, ",") {
				if f = strings.TrimSpace(f); f != "" {
					q.Fields = append(q.Fields, f)
				}
			}
		case "-h", "--help":
			return "", q, errors.New("用法: find <集合> [过滤JSON] [-sort 字段:-1] [-limit N] [-skip N] [-fields a,b]")
		default:
			if strings.HasPrefix(flagName, "-") {
				return "", q, fmt.Errorf("未知参数 %q", flagName)
			}
			pos = append(pos, flagName)
		}
	}
	if len(pos) == 0 {
		return "", q, errors.New("用法: find <集合> [过滤JSON] [-sort 字段:-1] [-limit N] [-skip N] [-fields a,b]")
	}
	if len(pos) > 1 {
		filter, err := parseObject(pos[1])
		if err != nil {
			return "", q, err
		}
		q.Filter = filter
	}
	if len(pos) > 2 {
		return "", q, fmt.Errorf("多余的参数 %q", pos[2])
	}
	return pos[0], q, nil
}

func parseObject(s string) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if doc == nil {
		return nil, errors.New("需要一个 JSON 对象")
	}
	return doc, nil
}

func printJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
func runShell(db *engine.DB) error {
	return runShellIO(db, os.Stdin, os.Stdout, os.Stderr)
}

func runShellIO(db *engine.DB, in io.Reader, out, errOut io.Writer) error {
	fmt.Fprintf(out, "NanoDB %s 交互式 shell。JSON 参数请用单引号包裹。\n", engine.Version)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var tx *engine.Tx
	for {
		if tx != nil {
			fmt.Fprint(out, "nano(tx)> ")
		} else {
			fmt.Fprint(out, "nano> ")
		}
		if !sc.Scan() {
			if tx != nil {
				_ = tx.Rollback()
				fmt.Fprintln(out, "未提交的事务已回滚")
			}
			fmt.Fprintln(out)
			if err := sc.Err(); err != nil {
				return fmt.Errorf("读取输入失败: %w", err)
			}
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		args := splitArgs(line)
		if len(args) == 0 {
			continue
		}
		switch args[0] {
		case "exit", "quit", "\\q":
			if tx != nil {
				_ = tx.Rollback()
				fmt.Fprintln(out, "未提交的事务已回滚")
			}
			return nil
		case "help", "?":
			shellHelp(out)
			continue
		case "serve":
			fmt.Fprintln(out, "请退出 shell 后运行: nanodb -db <目录> serve")
			continue
		case "begin":
			if tx != nil {
				fmt.Fprintln(errOut, "错误: 已有进行中的事务")
				continue
			}
			t, err := db.Begin()
			if err != nil {
				fmt.Fprintln(errOut, "错误:", err)
				continue
			}
			tx = t
			fmt.Fprintln(out, "事务已开始")
			continue
		case "commit":
			if tx == nil {
				fmt.Fprintln(errOut, "错误: 没有进行中的事务")
				continue
			}
			err := tx.Commit()
			tx = nil
			if err != nil {
				fmt.Fprintln(errOut, "提交失败，事务已结束:", err)
				continue
			}
			fmt.Fprintln(out, "事务已提交")
			continue
		case "rollback":
			if tx == nil {
				fmt.Fprintln(errOut, "错误: 没有进行中的事务")
				continue
			}
			_ = tx.Rollback()
			tx = nil
			fmt.Fprintln(out, "事务已回滚")
			continue
		}
		store := dataStore(db)
		if tx != nil {
			store = tx
		}
		if err := dispatch(store, db, in, out, errOut, args[0], args[1:]); err != nil {
			fmt.Fprintln(errOut, "错误:", err)
		}
	}
}

func shellHelp(w io.Writer) {
	fmt.Fprint(w, `可用命令:
  put <集合> '<JSON>'          插入文档
  get <集合> <id>              读取文档
  find <集合> ['<过滤JSON>']   查询，可加 -sort/-limit/-skip/-fields
  count <集合> ['<过滤JSON>']  统计数量
  update <集合> <id> '<更新JSON>'
  delete <集合> <id>           删除文档
  drop <集合>                  删除集合
  index create <集合> <字段> [unique]  创建二级索引
  index drop <集合> <字段>     删除索引
  index list <集合>            查看索引
  batch '<操作JSON数组>'       原子批量写入
  begin                        开启事务
  commit                       提交事务（冲突时丢弃全部改动）
  rollback                     回滚事务
  collections                  列出集合
  stats                        统计信息
  compact                      压缩存储
  help                         显示帮助
  exit                         退出
`)
}

// splitArgs 按空白切分命令行，支持单引号与双引号包裹参数。
func splitArgs(line string) []string {
	var (
		out    []string
		b      strings.Builder
		quote  rune
		inWord bool
	)
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			inWord = true
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case unicode.IsSpace(r):
			if inWord {
				out = append(out, b.String())
				b.Reset()
				inWord = false
			}
		default:
			b.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, b.String())
	}
	return out
}

func serve(db *engine.DB, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", ":8080", "HTTP 监听地址")
	token := fs.String("token", "", "访问令牌（Bearer），为空则不鉴权")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	users, err := loadUserStore(db.Dir())
	if err != nil {
		return err
	}
	return serveHTTP(ctx, db, *addr, *token, users, log.Printf, nil)
}

// loadUserStore 只在 users.json 存在时启用用户体系，
// 避免默认的免鉴权 serve 变成所有请求 401。
func loadUserStore(dir string) (*server.UserStore, error) {
	path := filepath.Join(dir, "users.json")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return server.OpenUserStore(path)
}

// runUserCommand 管理 HTTP 账号，直接读写 <数据目录>/users.json。
func runUserCommand(path string, args []string, out io.Writer) error {
	store, err := server.OpenUserStore(path)
	if err != nil {
		return err
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		if len(args) < 2 {
			return errors.New("用法: user add <名字> [admin|write|read] [集合1,集合2]")
		}
		role := server.RoleWrite
		if len(args) > 2 {
			role = args[2]
		}
		var colls []string
		if len(args) > 3 {
			colls = strings.Split(args[3], ",")
		}
		u, err := store.Add(args[1], role, colls)
		if err != nil {
			return err
		}
		return printJSON(out, u)
	case "list":
		if len(args) > 1 {
			return errors.New("用法: user list")
		}
		return printJSON(out, store.List())
	case "remove", "delete":
		if len(args) < 2 {
			return errors.New("用法: user remove <名字>")
		}
		if err := store.Remove(args[1]); err != nil {
			return err
		}
		return printJSON(out, map[string]any{"deleted": args[1]})
	default:
		return errors.New("用法: user add|list|remove ...")
	}
}

// serveHTTP 启动 HTTP 服务并等待 ctx 取消或服务退出；ready 用于测试回调实际监听地址。
func serveHTTP(ctx context.Context, db *engine.DB, addr, token string, users *server.UserStore, logf func(string, ...any), ready func(string)) error {
	if logf == nil {
		logf = log.Printf
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	api := server.NewWithOptions(db, server.Options{Token: token, Users: users})
	defer api.Close()
	srv := &http.Server{
		Handler:           api,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	actual := ln.Addr().String()
	logf("NanoDB HTTP 服务已启动: http://%s", displayAddr(actual))
	if ready != nil {
		ready(actual)
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		logf("NanoDB HTTP 服务已停止")
	}
	return nil
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
