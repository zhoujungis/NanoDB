package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	// ErrTxDone 表示事务已提交或回滚，不能再继续使用。
	ErrTxDone = errors.New("nanodb: transaction already finished")
	// ErrConflict 表示事务期间读写集合被其他写入修改，提交被拒绝。
	ErrConflict = errors.New("nanodb: transaction conflict")
)

// Tx 是一个显式事务。
//
// 事务采用乐观并发控制：首次访问某个集合时克隆其快照，事务内的读写都基于
// 该快照（可重复读）；提交时若发现事务访问过的集合在事务期间被其他写入修改，
// 则返回 ErrConflict 并放弃全部改动（只读事务同样会校验快照一致性）。
// Commit 成功时整批改动只写一条 WAL 记录。
//
// 隔离级别与冲突语义详见 docs/transaction-isolation.md。
//
// Tx 的方法可被多个 goroutine 并发调用。
type Tx struct {
	mu sync.Mutex

	db       *DB
	snap     map[string]*collection // 事务内集合快照
	versions map[string]uint64      // 各集合首次访问时的版本
	writes   map[string]bool        // 事务内发生过写入的集合
	walOps   []walRecord
	done     bool
}

// Begin 开启一个显式事务。事务不会持有数据库锁，直到 Commit 时才短暂加写锁。
func (db *DB) Begin() (*Tx, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if err := db.checkOpen(); err != nil {
		return nil, err
	}
	return &Tx{
		db:       db,
		snap:     make(map[string]*collection),
		versions: make(map[string]uint64),
		writes:   make(map[string]bool),
	}, nil
}

func (tx *Tx) check() error {
	if tx.done {
		return ErrTxDone
	}
	return nil
}

// collection 返回事务内的集合快照，首次访问时按需克隆。
// 调用方必须已持有 tx.mu。
func (tx *Tx) collection(name string) (*collection, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: empty collection name", ErrInvalid)
	}
	if c := tx.snap[name]; c != nil {
		return c, nil
	}
	tx.db.mu.RLock()
	defer tx.db.mu.RUnlock()
	if err := tx.db.checkOpen(); err != nil {
		return nil, err
	}
	tx.versions[name] = tx.db.versions[name]
	if live := tx.db.colls[name]; live != nil {
		c := live.clone()
		tx.snap[name] = c
		return c, nil
	}
	c := newCollection()
	tx.snap[name] = c
	return c, nil
}

func (tx *Tx) finishLocked() {
	tx.done = true
	tx.snap = nil
	tx.versions = nil
	tx.writes = nil
	tx.walOps = nil
}

// Commit 提交事务。若事务访问过的集合在事务期间被修改，返回 ErrConflict。
func (tx *Tx) Commit() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return err
	}

	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	if err := tx.db.checkOpen(); err != nil {
		tx.finishLocked()
		return err
	}
	// 校验事务访问过的整个读集，避免丢失更新与不可重复读；
	// 只读事务同样校验快照一致性。
	for name, ver := range tx.versions {
		if tx.db.versions[name] != ver {
			tx.finishLocked()
			return fmt.Errorf("%w: collection %q changed during transaction", ErrConflict, name)
		}
	}
	if len(tx.walOps) > 0 {
		if err := tx.db.appendRecord(walRecord{Op: "tx", Ops: tx.walOps}); err != nil {
			tx.finishLocked()
			return err
		}
	}
	for name := range tx.writes {
		tx.db.colls[name] = tx.snap[name]
		tx.db.touchLocked(name)
	}
	tx.finishLocked()
	tx.db.maybeCompactLocked()
	return nil
}

// Rollback 放弃事务内的所有改动。
func (tx *Tx) Rollback() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return err
	}
	tx.finishLocked()
	return nil
}

// Get 按 _id 读取事务快照中的文档，返回副本。
func (tx *Tx) Get(coll, id string) (map[string]any, bool) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return nil, false
	}
	c, err := tx.collection(coll)
	if err != nil {
		return nil, false
	}
	doc, ok := c.docs[id]
	if !ok {
		return nil, false
	}
	return deepCopyDoc(doc), true
}

// Find 在事务快照上执行查询，能看到事务内尚未提交的写入。
func (tx *Tx) Find(coll string, q Query) ([]map[string]any, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return nil, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return nil, err
	}
	return queryCollection(c, q), nil
}

// Count 统计事务快照中满足条件的文档数量。
func (tx *Tx) Count(coll string, filter map[string]any) (int, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return 0, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return 0, err
	}
	return countCollection(c, filter), nil
}

func (tx *Tx) insertLocked(coll string, doc map[string]any) (map[string]any, error) {
	if strings.TrimSpace(coll) == "" {
		return nil, fmt.Errorf("%w: empty collection name", ErrInvalid)
	}
	d, id, err := prepareInsertDoc(doc)
	if err != nil {
		return nil, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return nil, err
	}
	if err := c.add(id, d); err != nil {
		return nil, err
	}
	tx.writes[coll] = true
	tx.walOps = append(tx.walOps, walRecord{Op: "put", Coll: coll, Doc: d})
	return deepCopyDoc(d), nil
}

// Insert 在事务内插入文档。
func (tx *Tx) Insert(coll string, doc map[string]any) (map[string]any, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return nil, err
	}
	return tx.insertLocked(coll, doc)
}

// InsertMany 在事务内依次插入多个文档，返回成功插入的副本。
func (tx *Tx) InsertMany(coll string, docs []map[string]any) ([]map[string]any, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		d, err := tx.insertLocked(coll, doc)
		if err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Replace 在事务内整体替换文档，_id 不可更改。
func (tx *Tx) Replace(coll, id string, doc map[string]any) (map[string]any, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return nil, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return nil, err
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
	tx.writes[coll] = true
	tx.walOps = append(tx.walOps, walRecord{Op: "put", Coll: coll, Doc: d})
	return deepCopyDoc(d), nil
}

// Update 在事务内使用更新操作符修改单个文档。
func (tx *Tx) Update(coll, id string, patch map[string]any) (map[string]any, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return nil, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return nil, err
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
	tx.writes[coll] = true
	tx.walOps = append(tx.walOps, walRecord{Op: "put", Coll: coll, Doc: updated})
	return deepCopyDoc(updated), nil
}

// UpdateMany 在事务内更新所有匹配的文档，返回修改数量。
func (tx *Tx) UpdateMany(coll string, filter, patch map[string]any) (int, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return 0, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return 0, err
	}
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
		tx.writes[coll] = true
		tx.walOps = append(tx.walOps, walRecord{Op: "put", Coll: coll, Doc: updated})
		modified++
	}
	return modified, nil
}

// Delete 在事务内删除单个文档，返回是否确实删除。
func (tx *Tx) Delete(coll, id string) (bool, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return false, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return false, err
	}
	if _, ok := c.docs[id]; !ok {
		return false, nil
	}
	c.remove(id)
	tx.writes[coll] = true
	tx.walOps = append(tx.walOps, walRecord{Op: "delete", Coll: coll, ID: id})
	return true, nil
}

// DeleteMany 在事务内删除所有匹配的文档，返回删除数量。
func (tx *Tx) DeleteMany(coll string, filter map[string]any) (int, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return 0, err
	}
	c, err := tx.collection(coll)
	if err != nil {
		return 0, err
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
		c.remove(id)
		tx.writes[coll] = true
		tx.walOps = append(tx.walOps, walRecord{Op: "delete", Coll: coll, ID: id})
		deleted++
	}
	return deleted, nil
}

// Batch 在事务内原子执行一组批量操作；批次内任一步失败时本批次不生效。
func (tx *Tx) Batch(ops []BatchOp) ([]BatchResult, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if err := tx.check(); err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: empty batch", ErrInvalid)
	}

	staged := make(map[string]*collection)
	stagedColl := func(name string) (*collection, error) {
		if c := staged[name]; c != nil {
			return c, nil
		}
		base, err := tx.collection(name)
		if err != nil {
			return nil, err
		}
		c := base.clone()
		staged[name] = c
		return c, nil
	}

	results, walOps, err := applyBatchOps(stagedColl, ops)
	if err != nil {
		return nil, err
	}
	for name, c := range staged {
		tx.snap[name] = c
	}
	for _, rec := range walOps {
		tx.writes[rec.Coll] = true
	}
	tx.walOps = append(tx.walOps, walOps...)
	return results, nil
}
