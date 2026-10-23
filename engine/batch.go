package engine

import (
	"fmt"
	"strings"
)

// BatchOp 是批量操作中的一步，Op 支持 insert、update、delete。
type BatchOp struct {
	Op    string         `json:"op"`
	Coll  string         `json:"coll"`
	ID    string         `json:"id,omitempty"`
	Doc   map[string]any `json:"doc,omitempty"`
	Patch map[string]any `json:"patch,omitempty"`
}

// BatchResult 是一步批量操作的执行结果。
type BatchResult struct {
	Op      string         `json:"op"`
	Coll    string         `json:"coll"`
	ID      string         `json:"id,omitempty"`
	Doc     map[string]any `json:"doc,omitempty"`
	Deleted bool           `json:"deleted,omitempty"`
}

// Batch 原子执行一组写操作：任一操作失败则全部不生效。
// 成功时只写入一条 WAL 记录，崩溃恢复时整批一起重放。
func (db *DB) Batch(ops []BatchOp) ([]BatchResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
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
		if c := db.colls[name]; c != nil {
			c = c.clone()
			staged[name] = c
			return c, nil
		}
		c := newCollection()
		staged[name] = c
		return c, nil
	}

	results, walOps, err := applyBatchOps(stagedColl, ops)
	if err != nil {
		return nil, err
	}
	if len(walOps) > 0 {
		if err := db.appendRecord(walRecord{Op: "batch", Ops: walOps}); err != nil {
			return nil, err
		}
	}
	// 暂存区验证通过后才替换正式集合，保证失败时原数据完全不变。
	changed := make(map[string]struct{}, len(walOps))
	for _, rec := range walOps {
		changed[rec.Coll] = struct{}{}
	}
	for name := range changed {
		if c := staged[name]; c != nil {
			db.colls[name] = c
		}
		db.touchLocked(name)
	}
	db.maybeCompactLocked()
	return results, nil
}

// applyBatchOps 在给定集合视图上执行批量操作。
// get 负责按名称取得（或克隆/新建）可写集合；成功时返回结果与待写 WAL 记录。
func applyBatchOps(get func(string) (*collection, error), ops []BatchOp) ([]BatchResult, []walRecord, error) {
	results := make([]BatchResult, 0, len(ops))
	walOps := make([]walRecord, 0, len(ops))
	for i, op := range ops {
		coll := strings.TrimSpace(op.Coll)
		if coll == "" {
			return nil, nil, fmt.Errorf("%w: batch op %d has empty collection", ErrInvalid, i)
		}
		switch op.Op {
		case "insert":
			d, id, err := prepareInsertDoc(op.Doc)
			if err != nil {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, err)
			}
			c, err := get(coll)
			if err != nil {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, err)
			}
			if err := c.add(id, d); err != nil {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, err)
			}
			results = append(results, BatchResult{Op: "insert", Coll: coll, ID: id, Doc: deepCopyDoc(d)})
			walOps = append(walOps, walRecord{Op: "put", Coll: coll, Doc: d})

		case "update":
			if op.ID == "" {
				return nil, nil, fmt.Errorf("%w: batch op %d missing id", ErrInvalid, i)
			}
			c, err := get(coll)
			if err != nil {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, err)
			}
			old, exists := c.docs[op.ID]
			if !exists {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, ErrNotFound)
			}
			updated, err := applyUpdate(old, op.Patch)
			if err != nil {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, err)
			}
			updated["_id"] = op.ID
			if err := c.replace(op.ID, old, updated); err != nil {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, err)
			}
			results = append(results, BatchResult{Op: "update", Coll: coll, ID: op.ID, Doc: deepCopyDoc(updated)})
			walOps = append(walOps, walRecord{Op: "put", Coll: coll, Doc: updated})

		case "delete":
			if op.ID == "" {
				return nil, nil, fmt.Errorf("%w: batch op %d missing id", ErrInvalid, i)
			}
			c, err := get(coll)
			if err != nil {
				return nil, nil, fmt.Errorf("batch op %d: %w", i, err)
			}
			existed := c.docs[op.ID] != nil
			c.remove(op.ID)
			results = append(results, BatchResult{Op: "delete", Coll: coll, ID: op.ID, Deleted: existed})
			if existed {
				walOps = append(walOps, walRecord{Op: "delete", Coll: coll, ID: op.ID})
			}

		default:
			return nil, nil, fmt.Errorf("%w: batch op %d has unknown op %q", ErrInvalid, i, op.Op)
		}
	}
	return results, walOps, nil
}
