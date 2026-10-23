package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Version 是 NanoDB 的版本号。
const Version = "0.1.0"

const (
	snapshotName            = "snapshot.json"
	walName                 = "wal.log"
	defaultCompactThreshold = 4 << 20
	dirPerm                 = 0o755
	filePerm                = 0o644
)

var (
	// ErrNotFound 表示文档或集合不存在。
	ErrNotFound = errors.New("nanodb: document not found")
	// ErrDuplicateID 表示插入时 _id 已存在。
	ErrDuplicateID = errors.New("nanodb: duplicate _id")
	// ErrDuplicateKey 表示唯一索引冲突。
	ErrDuplicateKey = errors.New("nanodb: duplicate unique index key")
	// ErrClosed 表示数据库已关闭。
	ErrClosed = errors.New("nanodb: database closed")
	// ErrLocked 表示数据目录已被其他进程打开。
	ErrLocked = errors.New("nanodb: database directory is locked by another process")
	// ErrInvalid 表示调用方传入的参数不合法。
	ErrInvalid = errors.New("nanodb: invalid argument")
	// ErrInvalidUpdate 表示更新表达式不合法。
	ErrInvalidUpdate = errors.New("nanodb: invalid update")
)

// Options 控制打开数据库时的行为。
type Options struct {
	// Sync 为 true 时每次写入都会 fsync，保证掉电后已提交数据不丢失。
	Sync bool
	// CompactThreshold 是 WAL 字节数上限，超过后自动压缩为快照。
	CompactThreshold int64
}

// DB 是一个嵌入式文档数据库实例。同一个实例可被多个 goroutine 并发使用。
type DB struct {
	dir              string
	sync             bool
	compactThreshold int64

	mu       sync.RWMutex
	colls    map[string]*collection
	versions map[string]uint64 // 每个集合的写入版本，用于事务提交时的冲突检测
	lock     *os.File
	wal      *os.File
	walBytes int64
	walGen   uint64 // 当前 WAL 代数，启动时取自快照
	closed   bool
}

type collection struct {
	docs    map[string]map[string]any
	ids     []string
	indexes map[string]*index
	texts   map[string]*textIndex
	geos    map[string]*geoIndex
}

func newCollection() *collection {
	return &collection{
		docs:    make(map[string]map[string]any),
		indexes: make(map[string]*index),
		texts:   make(map[string]*textIndex),
		geos:    make(map[string]*geoIndex),
	}
}

func (c *collection) put(id string, doc map[string]any) {
	if _, ok := c.docs[id]; !ok {
		c.ids = append(c.ids, id)
	}
	c.docs[id] = doc
}

// add 写入新文档并维护索引，唯一索引冲突时返回 ErrDuplicateKey。
func (c *collection) add(id string, doc map[string]any) error {
	if _, exists := c.docs[id]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateID, id)
	}
	if err := c.checkUnique(id, doc); err != nil {
		return err
	}
	c.put(id, doc)
	for _, ix := range c.indexes {
		ix.add(id, doc)
	}
	c.addAux(id, doc)
	return nil
}

// addAux 维护文本与地理索引。
func (c *collection) addAux(id string, doc map[string]any) {
	for _, tx := range c.texts {
		tx.add(id, doc)
	}
	for _, gx := range c.geos {
		gx.add(id, doc)
	}
}

// removeAux 从文本与地理索引中删除文档。
func (c *collection) removeAux(id string, doc map[string]any) {
	for _, tx := range c.texts {
		tx.remove(id, doc)
	}
	for _, gx := range c.geos {
		gx.remove(id)
	}
}

// upsert 写入文档；已存在时按替换处理。用于 WAL 重放与批量操作。
func (c *collection) upsert(id string, doc map[string]any) error {
	if old, exists := c.docs[id]; exists {
		return c.replace(id, old, doc)
	}
	return c.add(id, doc)
}

// replace 整体替换文档并维护索引。
func (c *collection) replace(id string, oldDoc, newDoc map[string]any) error {
	if err := c.checkUnique(id, newDoc); err != nil {
		return err
	}
	for _, ix := range c.indexes {
		ix.remove(id, oldDoc)
	}
	c.removeAux(id, oldDoc)
	c.docs[id] = newDoc
	for _, ix := range c.indexes {
		ix.add(id, newDoc)
	}
	c.addAux(id, newDoc)
	return nil
}

func (c *collection) checkUnique(id string, doc map[string]any) error {
	for _, ix := range c.indexes {
		if err := ix.checkUnique(id, doc); err != nil {
			return err
		}
	}
	return nil
}

func (c *collection) remove(id string) {
	doc, ok := c.docs[id]
	if !ok {
		return
	}
	for _, ix := range c.indexes {
		ix.remove(id, doc)
	}
	c.removeAux(id, doc)
	delete(c.docs, id)
	for i, x := range c.ids {
		if x == id {
			c.ids = append(c.ids[:i], c.ids[i+1:]...)
			break
		}
	}
}

type walRecord struct {
	Op     string         `json:"op"`
	Gen    uint64         `json:"gen,omitempty"`
	Coll   string         `json:"coll"`
	ID     string         `json:"id,omitempty"`
	Doc    map[string]any `json:"doc,omitempty"`
	Ops    []walRecord    `json:"ops,omitempty"`
	Field  string         `json:"field,omitempty"`
	Fields []string       `json:"fields,omitempty"`
	Unique bool           `json:"unique,omitempty"`
	Type   string         `json:"type,omitempty"`
}

type snapshotData struct {
	Version int `json:"version"`
	// Gen 是快照对应的 WAL 代数：重放时只应用 Gen >= 该值的记录，
	// 避免压缩后 WAL 未被清空（或崩溃在 rename 与 truncate 之间）时，
	// 把快照已包含的旧记录再次应用，导致唯一索引冲突或数据错乱。
	Gen         uint64                      `json:"gen,omitempty"`
	Collections map[string][]map[string]any `json:"collections"`
	Indexes     map[string][]IndexSpec      `json:"indexes,omitempty"`
}

// Open 打开或创建位于 dir 的数据库。
func Open(dir string, opts *Options) (*DB, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: empty database directory", ErrInvalid)
	}
	o := Options{Sync: true, CompactThreshold: defaultCompactThreshold}
	if opts != nil {
		o.Sync = opts.Sync
		if opts.CompactThreshold > 0 {
			o.CompactThreshold = opts.CompactThreshold
		}
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("nanodb: create %s: %w", dir, err)
	}

	lockFile, err := acquireDirLock(dir)
	if err != nil {
		return nil, err
	}

	db := &DB{
		dir:              dir,
		sync:             o.Sync,
		compactThreshold: o.CompactThreshold,
		colls:            make(map[string]*collection),
		versions:         make(map[string]uint64),
		lock:             lockFile,
	}
	if err := db.loadSnapshot(); err != nil {
		releaseDirLock(lockFile)
		return nil, err
	}
	if err := db.openWAL(); err != nil {
		releaseDirLock(lockFile)
		return nil, err
	}
	if db.walBytes > db.compactThreshold {
		if err := db.compactLocked(); err != nil {
			_ = db.wal.Close()
			releaseDirLock(lockFile)
			return nil, err
		}
	}
	return db, nil
}

func (db *DB) loadSnapshot() error {
	data, err := os.ReadFile(filepath.Join(db.dir, snapshotName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("nanodb: read snapshot: %w", err)
	}
	var snap snapshotData
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("nanodb: corrupt snapshot: %w", err)
	}
	db.walGen = snap.Gen
	for name, docs := range snap.Collections {
		c := newCollection()
		for _, doc := range docs {
			id, ok := doc["_id"].(string)
			if !ok || id == "" {
				return fmt.Errorf("nanodb: snapshot document in %q has invalid _id", name)
			}
			c.put(id, doc)
		}
		db.colls[name] = c
	}
	for name, specs := range snap.Indexes {
		c := db.colls[name]
		if c == nil {
			c = newCollection()
			db.colls[name] = c
		}
		for _, spec := range specs {
			spec, err := normalizeIndexSpec(spec)
			if err != nil {
				return fmt.Errorf("nanodb: snapshot index in %q: %w", name, err)
			}
			if err := c.attachIndex(spec); err != nil {
				return fmt.Errorf("nanodb: rebuild index %q on %q: %w", spec.Field, name, err)
			}
		}
	}
	return nil
}

func (db *DB) openWAL() error {
	path := filepath.Join(db.dir, walName)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("nanodb: read wal: %w", err)
	}
	// 不使用 O_APPEND：Windows 下该标志会移除 GENERIC_WRITE，导致后续 Truncate 报 Access is denied。
	// 写入均由 db.mu 串行化，显式 Seek 到末尾即可保持追加语义。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return fmt.Errorf("nanodb: open wal: %w", err)
	}
	db.wal = f

	valid, err := db.replay(data)
	if err != nil {
		_ = f.Close()
		return err
	}
	if valid < len(data) {
		// 末尾未写完的记录直接截断，等效于该次写入未提交。
		if err := f.Truncate(int64(valid)); err != nil {
			_ = f.Close()
			return fmt.Errorf("nanodb: truncate torn wal: %w", err)
		}
	}
	if _, err := f.Seek(int64(valid), io.SeekStart); err != nil {
		_ = f.Close()
		return fmt.Errorf("nanodb: seek wal: %w", err)
	}
	db.walBytes = int64(valid)
	return nil
}

// replay 顺序重放 WAL，返回有效前缀长度。
func (db *DB) replay(data []byte) (int, error) {
	offset := 0
	for offset < len(data) {
		nl := bytes.IndexByte(data[offset:], '\n')
		if nl < 0 {
			return offset, nil
		}
		line := bytes.TrimSpace(data[offset : offset+nl])
		if len(line) > 0 {
			var rec walRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				return 0, fmt.Errorf("nanodb: corrupt wal near byte %d: %w", offset, err)
			}
			// 早于当前快照代数的记录已经被快照覆盖，直接跳过。
			if rec.Gen < db.walGen {
				offset += nl + 1
				continue
			}
			if err := db.applyRecord(rec); err != nil {
				return 0, err
			}
		}
		offset += nl + 1
	}
	return offset, nil
}

func (db *DB) applyRecord(rec walRecord) error {
	switch rec.Op {
	case "put":
		id, ok := rec.Doc["_id"].(string)
		if !ok || id == "" {
			return fmt.Errorf("nanodb: wal put record without valid _id")
		}
		if err := db.getCollection(rec.Coll).upsert(id, rec.Doc); err != nil {
			return fmt.Errorf("nanodb: replay put %s/%s: %w", rec.Coll, id, err)
		}
	case "delete":
		if c := db.colls[rec.Coll]; c != nil {
			c.remove(rec.ID)
		}
	case "drop":
		delete(db.colls, rec.Coll)
	case "batch", "tx":
		for i, sub := range rec.Ops {
			if err := db.applyRecord(sub); err != nil {
				return fmt.Errorf("nanodb: replay %s op %d: %w", rec.Op, i, err)
			}
		}
	case "index.create":
		spec, err := normalizeIndexSpec(IndexSpec{Field: rec.Field, Fields: rec.Fields, Unique: rec.Unique, Type: rec.Type})
		if err != nil {
			return fmt.Errorf("nanodb: wal index record: %w", err)
		}
		c := db.getCollection(rec.Coll)
		if err := c.attachIndex(spec); err != nil {
			return fmt.Errorf("nanodb: replay index %q on %q: %w", spec.Field, rec.Coll, err)
		}
	case "index.drop":
		if c := db.colls[rec.Coll]; c != nil {
			delete(c.indexes, rec.Field)
			delete(c.texts, rec.Field)
			delete(c.geos, rec.Field)
		}
	default:
		return fmt.Errorf("nanodb: unknown wal op %q", rec.Op)
	}
	return nil
}

func (db *DB) appendRecord(rec walRecord) error {
	if db.closed {
		return ErrClosed
	}
	rec.Gen = db.walGen
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("nanodb: encode wal record: %w", err)
	}
	data = append(data, '\n')
	if _, err := db.wal.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("nanodb: seek wal: %w", err)
	}
	n, err := db.wal.Write(data)
	if err != nil || n != len(data) {
		// 写失败可能在文件尾部留下半条记录，必须回退到上一个完整记录，
		// 否则后续追加会与残缺字节拼成一条非法记录，导致重启后无法打开。
		db.rollbackWALLocked()
		if err != nil {
			return fmt.Errorf("nanodb: write wal: %w", err)
		}
		return fmt.Errorf("nanodb: write wal: short write (%d/%d bytes)", n, len(data))
	}
	if db.sync {
		if err := db.wal.Sync(); err != nil {
			db.rollbackWALLocked()
			return fmt.Errorf("nanodb: sync wal: %w", err)
		}
	}
	db.walBytes += int64(len(data))
	return nil
}

// rollbackWALLocked 把 WAL 截断回最后一个完整记录的位置。
// 必须由持有 db.mu 写锁的调用方使用。
func (db *DB) rollbackWALLocked() {
	_ = db.wal.Truncate(db.walBytes)
	_, _ = db.wal.Seek(db.walBytes, io.SeekStart)
}

// maybeCompactLocked 在内存状态已完成后检查是否需要压缩。
// 不能在 appendRecord 内压缩，否则快照可能早于尚未应用的删除/建索引操作。
func (db *DB) maybeCompactLocked() {
	if db.walBytes > db.compactThreshold {
		_ = db.compactLocked()
	}
}

func (db *DB) getCollection(name string) *collection {
	c := db.colls[name]
	if c == nil {
		c = newCollection()
		db.colls[name] = c
	}
	return c
}

// touchLocked 记录集合的逻辑写入版本，供事务冲突检测使用。
// 必须由持有 db.mu 写锁的调用方使用。
func (db *DB) touchLocked(name string) {
	db.versions[name]++
}

func (db *DB) checkOpen() error {
	if db.closed {
		return ErrClosed
	}
	return nil
}

// Insert 插入一个文档。未提供 _id 时自动生成，返回带 _id 的副本。
func (db *DB) Insert(coll string, doc map[string]any) (map[string]any, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.insertLocked(coll, doc)
}

// prepareInsertDoc 复制文档并校验/生成 _id。
func prepareInsertDoc(doc map[string]any) (map[string]any, string, error) {
	if doc == nil {
		doc = map[string]any{}
	}
	d := normalizeDoc(doc)
	id, ok := d["_id"].(string)
	if !ok || id == "" {
		if _, present := d["_id"]; present {
			return nil, "", fmt.Errorf("%w: _id must be a non-empty string", ErrInvalid)
		}
		id = newID()
		d["_id"] = id
	}
	return d, id, nil
}

func (db *DB) insertLocked(coll string, doc map[string]any) (map[string]any, error) {
	if err := db.checkOpen(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(coll) == "" {
		return nil, fmt.Errorf("%w: empty collection name", ErrInvalid)
	}
	d, id, err := prepareInsertDoc(doc)
	if err != nil {
		return nil, err
	}

	c := db.getCollection(coll)
	if err := c.add(id, d); err != nil {
		return nil, err
	}
	if err := db.appendRecord(walRecord{Op: "put", Coll: coll, Doc: d}); err != nil {
		c.remove(id)
		return nil, err
	}
	db.touchLocked(coll)
	db.maybeCompactLocked()
	return deepCopyDoc(d), nil
}

// InsertMany 依次插入多个文档，返回成功插入的副本。
func (db *DB) InsertMany(coll string, docs []map[string]any) ([]map[string]any, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		d, err := db.insertLocked(coll, doc)
		if err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Get 按 _id 读取文档，返回副本。
func (db *DB) Get(coll, id string) (map[string]any, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false
	}
	c := db.colls[coll]
	if c == nil {
		return nil, false
	}
	doc, ok := c.docs[id]
	if !ok {
		return nil, false
	}
	return deepCopyDoc(doc), true
}

// Find 按条件查询文档，返回副本列表；结果按插入顺序稳定排列。
func (db *DB) Find(coll string, q Query) ([]map[string]any, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	return queryCollection(db.colls[coll], q), nil
}

// queryCollection 在给定集合上执行查询，返回深拷贝与投影后的结果。
func queryCollection(c *collection, q Query) []map[string]any {
	out := make([]map[string]any, 0)
	if c == nil {
		return out
	}
	candidates := c.planCandidates(q.Filter)
	clauses := compileFilter(q.Filter)
	for _, id := range c.ids {
		if candidates != nil {
			if _, ok := candidates[id]; !ok {
				continue
			}
		}
		doc := c.docs[id]
		if doc == nil {
			continue
		}
		if clauses != nil && !matchClauses(doc, clauses) {
			continue
		}
		out = append(out, doc)
	}
	if len(q.Sort) > 0 {
		if q.Limit > 0 {
			k := len(out)
			if q.Skip < len(out) && q.Limit < len(out)-q.Skip {
				k = q.Skip + q.Limit
			}
			out = topNSortDocs(out, q.Sort, k)
		} else {
			sortDocs(out, q.Sort)
		}
	}

	if q.Skip > 0 {
		if q.Skip >= len(out) {
			out = nil
		} else {
			out = out[q.Skip:]
		}
	}
	if q.Limit > 0 && q.Limit < len(out) {
		out = out[:q.Limit]
	}

	result := make([]map[string]any, 0, len(out))
	if len(q.Fields) > 0 {
		for _, doc := range out {
			result = append(result, copyProjectedDoc(doc, q.Fields))
		}
		return result
	}
	for _, doc := range out {
		result = append(result, deepCopyDoc(doc))
	}
	return result
}

// Count 返回满足条件的文档数量。
func (db *DB) Count(coll string, filter map[string]any) (int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return 0, ErrClosed
	}
	return countCollection(db.colls[coll], filter), nil
}

// countCollection 统计给定集合中满足条件的文档数。
func countCollection(c *collection, filter map[string]any) int {
	if c == nil {
		return 0
	}
	if filter == nil {
		return len(c.docs)
	}
	clauses := compileFilter(filter)
	n := 0
	if candidates := c.planCandidates(filter); candidates != nil {
		for id := range candidates {
			doc := c.docs[id]
			if doc != nil && matchClauses(doc, clauses) {
				n++
			}
		}
		return n
	}
	for _, doc := range c.docs {
		if matchClauses(doc, clauses) {
			n++
		}
	}
	return n
}

// Replace 用新文档整体替换已有文档，_id 不可更改。
func (db *DB) Replace(coll, id string, doc map[string]any) (map[string]any, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return nil, err
	}
	c := db.colls[coll]
	if c == nil {
		return nil, ErrNotFound
	}
	old, ok := c.docs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if doc == nil {
		doc = map[string]any{}
	}
	d := normalizeDoc(doc)
	d["_id"] = id
	if err := c.replace(id, old, d); err != nil {
		return nil, err
	}
	if err := db.appendRecord(walRecord{Op: "put", Coll: coll, Doc: d}); err != nil {
		_ = c.replace(id, d, old)
		return nil, err
	}
	db.touchLocked(coll)
	db.maybeCompactLocked()
	return deepCopyDoc(d), nil
}

// Update 使用更新操作符修改单个文档。
func (db *DB) Update(coll, id string, patch map[string]any) (map[string]any, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return nil, err
	}
	c := db.colls[coll]
	if c == nil {
		return nil, ErrNotFound
	}
	doc, ok := c.docs[id]
	if !ok {
		return nil, ErrNotFound
	}
	updated, err := applyUpdate(doc, patch)
	if err != nil {
		return nil, err
	}
	updated["_id"] = id
	if err := c.replace(id, doc, updated); err != nil {
		return nil, err
	}
	if err := db.appendRecord(walRecord{Op: "put", Coll: coll, Doc: updated}); err != nil {
		_ = c.replace(id, updated, doc)
		return nil, err
	}
	db.touchLocked(coll)
	db.maybeCompactLocked()
	return deepCopyDoc(updated), nil
}

// UpdateMany 更新所有匹配的文档，返回修改数量。
func (db *DB) UpdateMany(coll string, filter, patch map[string]any) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return 0, err
	}
	c := db.colls[coll]
	if c == nil {
		return 0, nil
	}
	// 先做一次空文档校验，避免执行到一半才发现表达式错误。
	if _, err := applyUpdate(map[string]any{}, patch); err != nil {
		return 0, err
	}
	ids := append([]string(nil), c.ids...)
	clauses := compileFilter(filter)
	modified := 0
	for _, id := range ids {
		doc := c.docs[id]
		if doc == nil {
			continue
		}
		if clauses != nil && !matchClauses(doc, clauses) {
			continue
		}
		updated, err := applyUpdate(doc, patch)
		if err != nil {
			return modified, err
		}
		updated["_id"] = id
		if err := c.replace(id, doc, updated); err != nil {
			return modified, err
		}
		if err := db.appendRecord(walRecord{Op: "put", Coll: coll, Doc: updated}); err != nil {
			_ = c.replace(id, updated, doc)
			return modified, err
		}
		modified++
	}
	if modified > 0 {
		db.touchLocked(coll)
	}
	db.maybeCompactLocked()
	return modified, nil
}

// Delete 删除单个文档，返回是否确实删除。
func (db *DB) Delete(coll, id string) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return false, err
	}
	c := db.colls[coll]
	if c == nil {
		return false, nil
	}
	if _, ok := c.docs[id]; !ok {
		return false, nil
	}
	if err := db.appendRecord(walRecord{Op: "delete", Coll: coll, ID: id}); err != nil {
		return false, err
	}
	c.remove(id)
	db.touchLocked(coll)
	db.maybeCompactLocked()
	return true, nil
}

// DeleteMany 删除所有匹配的文档，返回删除数量。
func (db *DB) DeleteMany(coll string, filter map[string]any) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return 0, err
	}
	c := db.colls[coll]
	if c == nil {
		return 0, nil
	}
	ids := append([]string(nil), c.ids...)
	clauses := compileFilter(filter)
	deleted := 0
	for _, id := range ids {
		doc := c.docs[id]
		if doc == nil {
			continue
		}
		if clauses != nil && !matchClauses(doc, clauses) {
			continue
		}
		if err := db.appendRecord(walRecord{Op: "delete", Coll: coll, ID: id}); err != nil {
			return deleted, err
		}
		c.remove(id)
		deleted++
	}
	if deleted > 0 {
		db.touchLocked(coll)
	}
	db.maybeCompactLocked()
	return deleted, nil
}

// DropCollection 删除整个集合。
func (db *DB) DropCollection(coll string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return err
	}
	if strings.TrimSpace(coll) == "" {
		return fmt.Errorf("%w: empty collection name", ErrInvalid)
	}
	if _, ok := db.colls[coll]; !ok {
		return nil
	}
	if err := db.appendRecord(walRecord{Op: "drop", Coll: coll}); err != nil {
		return err
	}
	delete(db.colls, coll)
	db.touchLocked(coll)
	db.maybeCompactLocked()
	return nil
}

// CollectionInfo 描述一个集合的名称与文档数。
type CollectionInfo struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Collections 返回按名称排序的集合列表。
func (db *DB) Collections() []CollectionInfo {
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := make([]CollectionInfo, 0, len(db.colls))
	for name, c := range db.colls {
		out = append(out, CollectionInfo{Name: name, Count: len(c.docs)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Stats 是数据库的运行统计信息。
type Stats struct {
	Dir         string `json:"dir"`
	Collections int    `json:"collections"`
	Documents   int    `json:"documents"`
	Indexes     int    `json:"indexes"`
	WALBytes    int64  `json:"wal_bytes"`
	Sync        bool   `json:"sync"`
}

// Stats 返回当前统计信息。
func (db *DB) Stats() Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	s := Stats{Dir: db.dir, Collections: len(db.colls), WALBytes: db.walBytes, Sync: db.sync}
	for _, c := range db.colls {
		s.Documents += len(c.docs)
		s.Indexes += len(c.indexes)
	}
	return s
}

// Compact 将内存数据写入快照并清空 WAL。
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.compactLocked()
}

func (db *DB) compactLocked() error {
	if db.closed {
		return ErrClosed
	}
	if db.wal == nil {
		return fmt.Errorf("nanodb: wal is not open")
	}
	snap := snapshotData{
		Version:     1,
		Gen:         db.walGen + 1,
		Collections: make(map[string][]map[string]any, len(db.colls)),
		Indexes:     make(map[string][]IndexSpec),
	}
	for name, c := range db.colls {
		if len(c.docs) > 0 {
			docs := make([]map[string]any, 0, len(c.ids))
			for _, id := range c.ids {
				if d, ok := c.docs[id]; ok {
					docs = append(docs, d)
				}
			}
			snap.Collections[name] = docs
		}
		if specs := c.indexSpecs(); len(specs) > 0 {
			snap.Indexes[name] = specs
		}
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("nanodb: encode snapshot: %w", err)
	}

	path := filepath.Join(db.dir, snapshotName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("nanodb: create snapshot: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("nanodb: write snapshot: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("nanodb: sync snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("nanodb: close snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("nanodb: replace snapshot: %w", err)
	}
	// 快照已落盘，之后的写入使用新代数；即使接下来的 WAL 清空失败，
	// 旧代数的记录也会在重放时被跳过，不会重复叠加到快照之上。
	db.walGen = snap.Gen
	if err := db.wal.Truncate(0); err != nil {
		return fmt.Errorf("nanodb: truncate wal: %w", err)
	}
	// 截断成功后文件长度已是 0，后续追加/回滚都以该值为基准。
	db.walBytes = 0
	if _, err := db.wal.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("nanodb: seek wal: %w", err)
	}
	if db.sync {
		if err := db.wal.Sync(); err != nil {
			return fmt.Errorf("nanodb: sync wal: %w", err)
		}
	}
	return nil
}

// Dir 返回数据目录路径。
func (db *DB) Dir() string { return db.dir }

// Close 关闭数据库，释放 WAL 文件句柄与目录锁。
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	var err error
	if db.wal != nil {
		err = db.wal.Close()
		db.wal = nil
	}
	releaseDirLock(db.lock)
	db.lock = nil
	return err
}
