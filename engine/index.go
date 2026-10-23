package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// IndexSpec 描述一个二级索引。
type IndexSpec struct {
	Field  string `json:"field"`
	Unique bool   `json:"unique"`
}

// index 是一个有序的单字段二级索引。
// 使用有序条目支持等值、$in 与范围查询，数组字段按 multikey 方式逐元素索引。
type index struct {
	field   string
	unique  bool
	entries []*indexEntry
	byKey   map[string]*indexEntry
}

type indexEntry struct {
	key any
	ids map[string]struct{}
}

func newIndex(spec IndexSpec) *index {
	return &index{
		field:  spec.Field,
		unique: spec.Unique,
		byKey:  make(map[string]*indexEntry),
	}
}

func (ix *index) clone() *index {
	c := newIndex(IndexSpec{Field: ix.field, Unique: ix.unique})
	c.entries = make([]*indexEntry, len(ix.entries))
	for i, e := range ix.entries {
		ids := make(map[string]struct{}, len(e.ids))
		for id := range e.ids {
			ids[id] = struct{}{}
		}
		ne := &indexEntry{key: e.key, ids: ids}
		c.entries[i] = ne
		c.byKey[indexKeyString(e.key)] = ne
	}
	return c
}

// canonicalKey 将可索引的标量规范化为 map 键和比较值。
func canonicalKey(v any) (string, any, bool) {
	switch t := v.(type) {
	case string:
		return "s:" + t, t, true
	case bool:
		if t {
			return "b:1", true, true
		}
		return "b:0", false, true
	default:
		if f, ok := toFloat(t); ok {
			// 把 -0 规范化为 +0：两者按数值相等，必须落在同一个索引键上，
			// 否则等值查询会漏掉用 -0 建索引的文档。
			if f == 0 {
				f = 0
			}
			return "n:" + strconv.FormatFloat(f, 'g', -1, 64), f, true
		}
	}
	return "", nil, false
}

func indexKeyString(key any) string {
	k, _, _ := canonicalKey(key)
	return k
}

// keys 返回文档在该索引字段上的索引键，数组字段返回去重后的元素。
func (ix *index) keys(doc map[string]any) []any {
	v, ok := getPath(doc, ix.field)
	if !ok || v == nil {
		return nil
	}
	if arr, ok := asArray(v); ok {
		out := make([]any, 0, len(arr))
		seen := make(map[string]struct{}, len(arr))
		for _, item := range arr {
			if item == nil {
				continue
			}
			k, _, ok := canonicalKey(item)
			if !ok {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, item)
		}
		return out
	}
	if _, _, ok := canonicalKey(v); !ok {
		return nil
	}
	return []any{v}
}

func (ix *index) add(id string, doc map[string]any) {
	for _, raw := range ix.keys(doc) {
		ck, key, ok := canonicalKey(raw)
		if !ok {
			continue
		}
		e := ix.byKey[ck]
		if e == nil {
			e = &indexEntry{key: key, ids: make(map[string]struct{})}
			ix.byKey[ck] = e
			pos := sort.Search(len(ix.entries), func(i int) bool {
				return compareIndexKeys(ix.entries[i].key, key) >= 0
			})
			ix.entries = append(ix.entries, nil)
			copy(ix.entries[pos+1:], ix.entries[pos:])
			ix.entries[pos] = e
		}
		e.ids[id] = struct{}{}
	}
}

func (ix *index) remove(id string, doc map[string]any) {
	for _, raw := range ix.keys(doc) {
		ck, _, ok := canonicalKey(raw)
		if !ok {
			continue
		}
		e := ix.byKey[ck]
		if e == nil {
			continue
		}
		delete(e.ids, id)
		if len(e.ids) > 0 {
			continue
		}
		delete(ix.byKey, ck)
		for i, x := range ix.entries {
			if x == e {
				ix.entries = append(ix.entries[:i], ix.entries[i+1:]...)
				break
			}
		}
	}
}

func (ix *index) checkUnique(id string, doc map[string]any) error {
	if !ix.unique {
		return nil
	}
	for _, raw := range ix.keys(doc) {
		ck, _, ok := canonicalKey(raw)
		if !ok {
			continue
		}
		e := ix.byKey[ck]
		if e == nil {
			continue
		}
		for other := range e.ids {
			if other != id {
				return fmt.Errorf("%w: index %q conflicts on value %v", ErrDuplicateKey, ix.field, raw)
			}
		}
	}
	return nil
}

// lookup 根据条件返回候选文档 ID 集合；返回 nil 表示该索引不适合此条件。
func (ix *index) lookup(cond any) map[string]struct{} {
	if ops, ok := cond.(map[string]any); ok {
		if !hasOperator(ops) {
			return nil
		}
		for op, val := range ops {
			switch op {
			case "$eq":
				return ix.lookupValue(val)
			case "$in":
				items, ok := asArray(val)
				if !ok {
					return nil
				}
				out := make(map[string]struct{})
				for _, item := range items {
					ids := ix.lookupValue(item)
					if ids == nil {
						// 含 null 等不可索引值，退回全表扫描。
						return nil
					}
					for id := range ids {
						out[id] = struct{}{}
					}
				}
				return out
			case "$gt", "$gte", "$lt", "$lte":
				return ix.lookupRange(op, val)
			}
		}
		return nil
	}
	return ix.lookupValue(cond)
}

func (ix *index) lookupValue(val any) map[string]struct{} {
	if val == nil {
		return nil
	}
	ck, _, ok := canonicalKey(val)
	if !ok {
		return nil
	}
	out := make(map[string]struct{})
	if e := ix.byKey[ck]; e != nil {
		for id := range e.ids {
			out[id] = struct{}{}
		}
	}
	return out
}

func (ix *index) lookupRange(op string, val any) map[string]struct{} {
	if val == nil {
		return nil
	}
	if _, _, ok := canonicalKey(val); !ok {
		return nil
	}
	// entries 已按 compareIndexKeys 全序排列，二分定位边界后只遍历命中的条目。
	var lo, hi int
	switch op {
	case "$gt":
		lo = sort.Search(len(ix.entries), func(i int) bool {
			return compareIndexKeys(ix.entries[i].key, val) > 0
		})
		hi = len(ix.entries)
	case "$gte":
		lo = sort.Search(len(ix.entries), func(i int) bool {
			return compareIndexKeys(ix.entries[i].key, val) >= 0
		})
		hi = len(ix.entries)
	case "$lt":
		lo = 0
		hi = sort.Search(len(ix.entries), func(i int) bool {
			return compareIndexKeys(ix.entries[i].key, val) >= 0
		})
	case "$lte":
		lo = 0
		hi = sort.Search(len(ix.entries), func(i int) bool {
			return compareIndexKeys(ix.entries[i].key, val) > 0
		})
	default:
		return nil
	}
	out := make(map[string]struct{})
	for _, e := range ix.entries[lo:hi] {
		for id := range e.ids {
			out[id] = struct{}{}
		}
	}
	return out
}

func keyRank(v any) int {
	switch v.(type) {
	case string:
		return 1
	case bool:
		return 2
	default:
		if _, ok := toFloat(v); ok {
			return 0
		}
		return 3
	}
}

func compareIndexKeys(a, b any) int {
	ra, rb := keyRank(a), keyRank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	if ab, ok := a.(bool); ok {
		bb, ok := b.(bool)
		if !ok {
			return 0
		}
		switch {
		case ab == bb:
			return 0
		case !ab:
			return -1
		default:
			return 1
		}
	}
	if cmp, ok := compareValues(a, b); ok {
		return cmp
	}
	return 0
}

// buildIndex 从集合现有文档构建索引，构建失败（如唯一冲突）时返回错误。
func buildIndex(c *collection, spec IndexSpec) (*index, error) {
	ix := newIndex(spec)
	for _, id := range c.ids {
		doc := c.docs[id]
		if doc == nil {
			continue
		}
		if err := ix.checkUnique(id, doc); err != nil {
			return nil, err
		}
		ix.add(id, doc)
	}
	return ix, nil
}

func (c *collection) indexSpecs() []IndexSpec {
	specs := make([]IndexSpec, 0, len(c.indexes))
	for _, ix := range c.indexes {
		specs = append(specs, IndexSpec{Field: ix.field, Unique: ix.unique})
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Field < specs[j].Field })
	return specs
}

// clone 深拷贝集合，供批量写入的原子暂存区使用。
func (c *collection) clone() *collection {
	nc := newCollection()
	nc.ids = append([]string(nil), c.ids...)
	nc.docs = make(map[string]map[string]any, len(c.docs))
	for id, doc := range c.docs {
		nc.docs[id] = deepCopyDoc(doc)
	}
	for field, ix := range c.indexes {
		nc.indexes[field] = ix.clone()
	}
	return nc
}

// planCandidates 从过滤条件中挑选候选集最小的索引，返回 nil 表示走全表扫描。
func (c *collection) planCandidates(filter map[string]any) map[string]struct{} {
	if filter == nil || len(c.indexes) == 0 {
		return nil
	}
	var best map[string]struct{}
	var walk func(map[string]any)
	walk = func(f map[string]any) {
		for key, cond := range f {
			switch key {
			case "$and":
				list, ok := asFilterList(cond)
				if !ok {
					continue
				}
				for _, sub := range list {
					walk(sub)
				}
			case "$or", "$not":
				// $or/$not 无法安全地只取单字段候选集。
			default:
				ix := c.indexes[key]
				if ix == nil {
					continue
				}
				ids := ix.lookup(cond)
				if ids == nil {
					continue
				}
				if best == nil || len(ids) < len(best) {
					best = ids
				}
			}
		}
	}
	walk(filter)
	return best
}

// CreateIndex 为集合创建单字段二级索引；unique 为 true 时强制唯一约束。
func (db *DB) CreateIndex(coll, field string, unique bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return err
	}
	if strings.TrimSpace(coll) == "" {
		return fmt.Errorf("%w: empty collection name", ErrInvalid)
	}
	if strings.TrimSpace(field) == "" {
		return fmt.Errorf("%w: empty index field", ErrInvalid)
	}
	c := db.getCollection(coll)
	if ix, exists := c.indexes[field]; exists {
		if ix.unique == unique {
			return nil
		}
		return fmt.Errorf("%w: index on %q already exists with different options", ErrInvalid, field)
	}
	spec := IndexSpec{Field: field, Unique: unique}
	ix, err := buildIndex(c, spec)
	if err != nil {
		return err
	}
	// 先登记内存索引再写 WAL，避免写成功后的自动压缩丢掉索引定义。
	c.indexes[field] = ix
	if err := db.appendRecord(walRecord{Op: "index.create", Coll: coll, Field: field, Unique: unique}); err != nil {
		delete(c.indexes, field)
		return err
	}
	db.touchLocked(coll)
	db.maybeCompactLocked()
	return nil
}

// DropIndex 删除集合上的指定索引，索引不存在时视为成功。
func (db *DB) DropIndex(coll, field string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return err
	}
	c := db.colls[coll]
	if c == nil {
		return nil
	}
	if _, exists := c.indexes[field]; !exists {
		return nil
	}
	if err := db.appendRecord(walRecord{Op: "index.drop", Coll: coll, Field: field}); err != nil {
		return err
	}
	delete(c.indexes, field)
	db.touchLocked(coll)
	db.maybeCompactLocked()
	return nil
}

// ListIndexes 返回集合上的索引定义。
func (db *DB) ListIndexes(coll string) []IndexSpec {
	db.mu.RLock()
	defer db.mu.RUnlock()
	c := db.colls[coll]
	if c == nil {
		return []IndexSpec{}
	}
	return c.indexSpecs()
}
