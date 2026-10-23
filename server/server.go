package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nanodb/engine"
)

const (
	maxBodyBytes  = 8 << 20
	txIdleTimeout = 30 * time.Minute
)

// dataStore 是 DB 与事务共有的数据读写能力。
type dataStore interface {
	Insert(coll string, doc map[string]any) (map[string]any, error)
	InsertMany(coll string, docs []map[string]any) ([]map[string]any, error)
	Get(coll, id string) (map[string]any, bool)
	Find(coll string, q engine.Query) ([]map[string]any, error)
	Count(coll string, filter map[string]any) (int, error)
	Replace(coll, id string, doc map[string]any) (map[string]any, error)
	Update(coll, id string, patch map[string]any) (map[string]any, error)
	Delete(coll, id string) (bool, error)
	DeleteMany(coll string, filter map[string]any) (int, error)
}

// Server 把 NanoDB 暴露为轻量 REST 服务。
type Server struct {
	db    *engine.DB
	token string
	users *UserStore

	mu  sync.Mutex
	txs map[string]*txSession // 显式事务会话
}

// Options 配置 HTTP 服务的鉴权方式。
type Options struct {
	// Token 是共享管理 token，保留为 admin 兼容入口；为空表示不启用该入口。
	Token string
	// Users 是用户账号存储；nil 表示只使用共享 token（或完全不鉴权）。
	Users *UserStore
}

// New 创建 HTTP 服务，token 为空表示不开启鉴权。
func New(db *engine.DB, token string) *Server {
	return NewWithOptions(db, Options{Token: token})
}

// NewWithOptions 创建带用户体系的 HTTP 服务。
// token 与 users 都为空时不启用鉴权（全部请求视为 admin）。
func NewWithOptions(db *engine.DB, o Options) *Server {
	return &Server{
		db:    db,
		token: o.Token,
		users: o.Users,
		txs:   make(map[string]*txSession),
	}
}

// Close 停止所有空闲计时器并回滚未结束的事务会话。
func (s *Server) Close() {
	s.mu.Lock()
	sessions := make([]*txSession, 0, len(s.txs))
	for id, sess := range s.txs {
		sess.timer.Stop()
		sessions = append(sessions, sess)
		delete(s.txs, id)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		_ = sess.tx.Rollback()
	}
}

// txSession 是服务端保存的一个显式事务。
type txSession struct {
	tx       *engine.Tx
	lastUsed time.Time
	timer    *time.Timer
}

// Handler 返回可直接交给 http.Server 的处理器。
func (s *Server) Handler() http.Handler { return s }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	if path == "healthz" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	p, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !allowRequest(p, r.Method, path) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	r = withPrincipal(r, p)
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    "NanoDB",
			"version": engine.Version,
			"status":  "ok",
		})
		return
	}
	parts := strings.Split(path, "/")
	if parts[0] != "v1" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.serveV1(w, r, parts[1:])
}

func (s *Server) serveV1(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch parts[0] {
	case "collections":
		s.serveCollections(w, r, parts[1:])
	case "admin":
		s.serveAdmin(w, r, parts[1:])
	case "batch":
		s.serveBatch(w, r, parts[1:])
	case "indexes":
		s.serveIndexes(w, r, parts[1:])
	case "transactions":
		s.serveTransactions(w, r, parts[1:])
	case "aggregate":
		s.serveAggregate(w, r, parts[1:])
	default:
		coll := parts[0]
		switch len(parts) {
		case 1:
			serveCollection(w, r, s.db, coll)
		case 2:
			serveDocument(w, r, s.db, coll, parts[1])
		default:
			writeError(w, http.StatusNotFound, "not found")
		}
	}
}

func (s *Server) serveCollections(w http.ResponseWriter, r *http.Request, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.db.Collections())
	case len(rest) == 1 && r.Method == http.MethodDelete:
		if err := s.db.DropCollection(rest[0]); err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dropped": rest[0]})
	default:
		methodNotAllowed(w, "GET, DELETE")
	}
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if rest[0] == "users" {
		s.serveUsers(w, r, rest[1:])
		return
	}
	if len(rest) != 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch rest[0] {
	case "compact":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		if err := s.db.Compact(); err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"compacted": true})
	case "stats":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		writeJSON(w, http.StatusOK, s.db.Stats())
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// serveUsers 管理用户账号：
//
//	GET    /v1/admin/users         列出用户（含 token）
//	POST   /v1/admin/users         新建用户 {"name","role","collections"}
//	DELETE /v1/admin/users/{name}  删除用户
func (s *Server) serveUsers(w http.ResponseWriter, r *http.Request, rest []string) {
	if s.users == nil {
		writeError(w, http.StatusBadRequest, "user store is not configured")
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.users.List())
	case len(rest) == 0 && r.Method == http.MethodPost:
		var body struct {
			Name        string   `json:"name"`
			Role        string   `json:"role"`
			Collections []string `json:"collections"`
		}
		if err := decodeBody(w, r, &body); err != nil {
			writeDecodeError(w, err)
			return
		}
		if body.Role == "" {
			body.Role = RoleWrite
		}
		u, err := s.users.Add(body.Name, body.Role, body.Collections)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, u)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		if err := s.users.Remove(rest[0]); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": rest[0]})
	default:
		methodNotAllowed(w, "GET, POST, DELETE")
	}
}

func (s *Server) serveBatch(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) != 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var ops []engine.BatchOp
	if err := decodeBody(w, r, &ops); err != nil {
		writeDecodeError(w, err)
		return
	}
	if len(ops) == 0 {
		writeError(w, http.StatusBadRequest, "empty batch")
		return
	}
	// 批量操作跨多个集合，集合白名单只能逐条校验。
	if p, ok := principalFrom(r); ok && len(p.collections) > 0 {
		for _, op := range ops {
			if !p.canCollection(op.Coll) {
				writeError(w, http.StatusForbidden, "forbidden collection: "+op.Coll)
				return
			}
		}
	}
	results, err := s.db.Batch(ops)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(results), "results": results})
}

// serveTransactions 提供会话式显式事务：
//
//	POST   /v1/transactions                     开启事务
//	POST   /v1/transactions/{id}/ops            在事务内执行批量写操作
//	GET    /v1/transactions/{id}/documents/{c}  事务内查询
//	POST   /v1/transactions/{id}/documents/{c}  事务内插入
//	GET    /v1/transactions/{id}/documents/{c}/{id}
//	PUT/PATCH/DELETE /v1/transactions/{id}/documents/{c}/{id}
//	POST   /v1/transactions/{id}/commit         提交
//	POST   /v1/transactions/{id}/rollback       回滚
//	DELETE /v1/transactions/{id}                回滚
func (s *Server) serveTransactions(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) == 0 {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		tx, err := s.db.Begin()
		if err != nil {
			writeDBError(w, err)
			return
		}
		id := newTxID()
		sess := &txSession{tx: tx, lastUsed: time.Now()}
		s.mu.Lock()
		s.txs[id] = sess
		// 空闲事务由独立定时器回收，避免遗弃的会话长期占用内存。
		sess.timer = time.AfterFunc(txIdleTimeout, func() { s.expireTx(id) })
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"tx_id": id})
		return
	}

	id := rest[0]
	switch {
	case len(rest) == 1:
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, "DELETE")
			return
		}
		s.finishTx(w, id, false)

	case len(rest) == 2 && rest[1] == "commit":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		s.finishTx(w, id, true)

	case len(rest) == 2 && rest[1] == "rollback":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		s.finishTx(w, id, false)

	case len(rest) == 2 && rest[1] == "ops":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		tx, ok := s.lookupTx(id)
		if !ok {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		var ops []engine.BatchOp
		if err := decodeBody(w, r, &ops); err != nil {
			writeDecodeError(w, err)
			return
		}
		if len(ops) == 0 {
			writeError(w, http.StatusBadRequest, "empty batch")
			return
		}
		results, err := tx.Batch(ops)
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(results), "results": results})

	case len(rest) >= 3 && rest[1] == "documents":
		tx, ok := s.lookupTx(id)
		if !ok {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		switch len(rest) {
		case 3:
			serveCollection(w, r, tx, rest[2])
		case 4:
			serveDocument(w, r, tx, rest[2], rest[3])
		default:
			writeError(w, http.StatusNotFound, "not found")
		}

	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) lookupTx(id string) (*engine.Tx, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.txs[id]
	if sess == nil {
		return nil, false
	}
	sess.lastUsed = time.Now()
	sess.timer.Reset(txIdleTimeout)
	return sess.tx, true
}

func (s *Server) finishTx(w http.ResponseWriter, id string, commit bool) {
	s.mu.Lock()
	sess := s.txs[id]
	if sess != nil {
		sess.timer.Stop()
		delete(s.txs, id)
	}
	s.mu.Unlock()
	if sess == nil {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	if commit {
		if err := sess.tx.Commit(); err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"committed": true, "tx_id": id})
		return
	}
	if err := sess.tx.Rollback(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rolled_back": true, "tx_id": id})
}

// expireTx 在事务空闲超时后回收会话。若期间有请求刷新过 lastUsed，则按剩余时间重新计时。
func (s *Server) expireTx(id string) {
	s.mu.Lock()
	sess := s.txs[id]
	if sess == nil {
		s.mu.Unlock()
		return
	}
	idle := time.Since(sess.lastUsed)
	if idle < txIdleTimeout {
		sess.timer.Reset(txIdleTimeout - idle)
		s.mu.Unlock()
		return
	}
	delete(s.txs, id)
	s.mu.Unlock()
	_ = sess.tx.Rollback()
}

func newTxID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("tx-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) serveIndexes(w http.ResponseWriter, r *http.Request, rest []string) {
	switch len(rest) {
	case 1:
		coll := rest[0]
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, s.db.ListIndexes(coll))
		case http.MethodPost:
			var spec engine.IndexSpec
			if err := decodeBody(w, r, &spec); err != nil {
				writeDecodeError(w, err)
				return
			}
			field := spec.Field
			if len(spec.Fields) > 0 {
				field = strings.Join(spec.Fields, ",")
			}
			if field == "" {
				writeError(w, http.StatusBadRequest, "field is required")
				return
			}
			if err := s.db.CreateIndex(coll, field, spec.Unique); err != nil {
				writeDBError(w, err)
				return
			}
			spec.Field = field
			writeJSON(w, http.StatusCreated, spec)
		default:
			methodNotAllowed(w, "GET, POST")
		}
	case 2:
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, "DELETE")
			return
		}
		coll, field := rest[0], rest[1]
		if err := s.db.DropIndex(coll, field); err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dropped": field})
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// serveAggregate 处理 POST /v1/aggregate/{集合}，请求体为 {"pipeline":[...]}。
func (s *Server) serveAggregate(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) != 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var body struct {
		Pipeline []map[string]any `json:"pipeline"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeDecodeError(w, err)
		return
	}
	docs, err := s.db.Aggregate(rest[0], body.Pipeline)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

func serveCollection(w http.ResponseWriter, r *http.Request, st dataStore, coll string) {
	switch r.Method {
	case http.MethodGet:
		q, err := parseQuery(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if isTruthy(r.URL.Query().Get("count")) {
			n, err := st.Count(coll, q.Filter)
			if err != nil {
				writeDBError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"count": n})
			return
		}
		docs, err := st.Find(coll, q)
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, docs)

	case http.MethodPost:
		var raw any
		if err := decodeBody(w, r, &raw); err != nil {
			writeDecodeError(w, err)
			return
		}
		switch v := raw.(type) {
		case map[string]any:
			doc, err := st.Insert(coll, v)
			if err != nil {
				writeDBError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, doc)
		case []any:
			docs := make([]map[string]any, 0, len(v))
			for _, item := range v {
				doc, ok := item.(map[string]any)
				if !ok {
					writeError(w, http.StatusBadRequest, "array items must be JSON objects")
					return
				}
				docs = append(docs, doc)
			}
			inserted, err := st.InsertMany(coll, docs)
			if err != nil {
				writeDBError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"inserted": len(inserted), "docs": inserted})
		default:
			writeError(w, http.StatusBadRequest, "body must be a JSON object or array")
		}

	case http.MethodDelete:
		var filter map[string]any
		if f := r.URL.Query().Get("filter"); strings.TrimSpace(f) != "" {
			if err := json.Unmarshal([]byte(f), &filter); err != nil {
				writeError(w, http.StatusBadRequest, "invalid filter: "+err.Error())
				return
			}
		}
		n, err := st.DeleteMany(coll, filter)
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})

	default:
		methodNotAllowed(w, "GET, POST, DELETE")
	}
}

func serveDocument(w http.ResponseWriter, r *http.Request, st dataStore, coll, id string) {
	switch r.Method {
	case http.MethodGet:
		doc, ok := st.Get(coll, id)
		if !ok {
			writeError(w, http.StatusNotFound, "document not found")
			return
		}
		writeJSON(w, http.StatusOK, doc)

	case http.MethodPut:
		var doc map[string]any
		if err := decodeBody(w, r, &doc); err != nil {
			writeDecodeError(w, err)
			return
		}
		out, err := st.Replace(coll, id, doc)
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPatch:
		var patch map[string]any
		if err := decodeBody(w, r, &patch); err != nil {
			writeDecodeError(w, err)
			return
		}
		out, err := st.Update(coll, id, patch)
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodDelete:
		ok, err := st.Delete(coll, id)
		if err != nil {
			writeDBError(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "document not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})

	default:
		methodNotAllowed(w, "GET, PUT, PATCH, DELETE")
	}
}

func parseQuery(r *http.Request) (engine.Query, error) {
	var q engine.Query
	params := r.URL.Query()

	if f := params.Get("filter"); strings.TrimSpace(f) != "" {
		if err := json.Unmarshal([]byte(f), &q.Filter); err != nil {
			return q, fmt.Errorf("invalid filter: %w", err)
		}
	}
	if s := params.Get("sort"); strings.TrimSpace(s) != "" {
		sort, err := engine.ParseSort(s)
		if err != nil {
			return q, err
		}
		q.Sort = sort
	}
	limit, err := intParam(params.Get("limit"))
	if err != nil {
		return q, fmt.Errorf("invalid limit: %w", err)
	}
	skip, err := intParam(params.Get("skip"))
	if err != nil {
		return q, fmt.Errorf("invalid skip: %w", err)
	}
	if limit < 0 || skip < 0 {
		return q, errors.New("limit and skip must be non-negative")
	}
	q.Limit, q.Skip = limit, skip

	if fields := params.Get("fields"); strings.TrimSpace(fields) != "" {
		for _, f := range strings.Split(fields, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				q.Fields = append(q.Fields, f)
			}
		}
	}
	return q, nil
}

func intParam(s string) (int, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after JSON value")
	}
	return nil
}

// writeDecodeError 把请求体解析错误映射为合适的 HTTP 状态码。
func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"encode response failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeDBError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, "document not found")
	case errors.Is(err, engine.ErrDuplicateID):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrDuplicateKey):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrTxDone):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrInvalid), errors.Is(err, engine.ErrInvalidUpdate):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, engine.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "database closed")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}
