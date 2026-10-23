package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 索引类型。空字符串表示普通的标量/复合索引。
const (
	IndexTypeBTree = ""
	IndexTypeText  = "text"
	IndexTypeGeo   = "geo"
)

// IndexSpec 描述一个二级索引。Fields 长度为 1 时是单字段索引，
// 大于 1 时是复合索引；Field 保存逗号拼接的字段名，兼容旧格式。
// Type 为空表示普通索引，"text" 为全文索引（字段固定为 "*"），
// "geo" 为地理位置索引（单字段）。
type IndexSpec struct {
	Field  string   `json:"field"`
	Fields []string `json:"fields,omitempty"`
	Unique bool     `json:"unique"`
	Type   string   `json:"type,omitempty"`
}

// MarshalJSON 保持单字段索引 {"field":"age"} 的旧输出，
// 复合索引额外带出 fields 数组，文本/地理索引额外带出 type。
func (s IndexSpec) MarshalJSON() ([]byte, error) {
	fields := s.fieldList()
	type wire struct {
		Field  string   `json:"field"`
		Fields []string `json:"fields,omitempty"`
		Unique bool     `json:"unique"`
		Type   string   `json:"type,omitempty"`
	}
	w := wire{Field: strings.Join(fields, ","), Unique: s.Unique, Type: s.Type}
	if len(fields) > 1 {
		w.Fields = fields
	}
	return json.Marshal(w)
}

// fieldList 返回规范化后的字段列表，Fields 优先于 Field。
func (s IndexSpec) fieldList() []string {
	raw := s.Fields
	if len(raw) == 0 {
		raw = strings.Split(s.Field, ",")
	}
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// normalizeIndexSpec 校验索引定义并统一 Field/Fields 两种写法。
func normalizeIndexSpec(spec IndexSpec) (IndexSpec, error) {
	typ := strings.ToLower(strings.TrimSpace(spec.Type))
	switch typ {
	case IndexTypeBTree, IndexTypeText, IndexTypeGeo:
	default:
		return IndexSpec{}, fmt.Errorf("%w: unknown index type %q", ErrInvalid, spec.Type)
	}
	fields := spec.fieldList()
	if typ == IndexTypeText {
		if len(fields) != 1 || fields[0] != "*" {
			return IndexSpec{}, fmt.Errorf("%w: text index must be created on field %q", ErrInvalid, "*")
		}
		if spec.Unique {
			return IndexSpec{}, fmt.Errorf("%w: text index cannot be unique", ErrInvalid)
		}
		return IndexSpec{Field: "*", Fields: []string{"*"}, Type: typ}, nil
	}
	if typ == IndexTypeGeo {
		if len(fields) != 1 {
			return IndexSpec{}, fmt.Errorf("%w: geo index requires exactly one field", ErrInvalid)
		}
		if spec.Unique {
			return IndexSpec{}, fmt.Errorf("%w: geo index cannot be unique", ErrInvalid)
		}
		return IndexSpec{Field: fields[0], Fields: fields, Type: typ}, nil
	}
	if len(fields) == 0 {
		return IndexSpec{}, fmt.Errorf("%w: empty index field", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if f == "*" {
			return IndexSpec{}, fmt.Errorf("%w: field %q is reserved for text indexes", ErrInvalid, f)
		}
		if _, dup := seen[f]; dup {
			return IndexSpec{}, fmt.Errorf("%w: duplicate index field %q", ErrInvalid, f)
		}
		seen[f] = struct{}{}
	}
	return IndexSpec{Field: strings.Join(fields, ","), Fields: fields, Unique: spec.Unique}, nil
}

// index 是一个有序的二级索引。单字段索引对数组字段按 multikey 展开，
// 复合索引按字段顺序组成元组键。
type index struct {
	fields  []string
	unique  bool
	entries []*indexEntry
	byKey   map[string]*indexEntry
	// incomplete 表示有文档带有数组字段、无法进入复合索引。
	// 此时该索引不再参与查询规划，避免漏查。
	incomplete bool
}

type indexEntry struct {
	key []any
	ids map[string]struct{}
}

func newIndex(spec IndexSpec) *index {
	return &index{
		fields: spec.fieldList(),
		unique: spec.Unique,
		byKey:  make(map[string]*indexEntry),
	}
}

// name 返回索引标识：单字段为字段名，复合索引为逗号拼接的字段名。
func (ix *index) name() string {
	return strings.Join(ix.fields, ",")
}

func (ix *index) clone() *index {
	c := newIndex(IndexSpec{Fields: ix.fields, Unique: ix.unique})
	c.incomplete = ix.incomplete
	c.entries = make([]*indexEntry, len(ix.entries))
	for i, e := range ix.entries {
		ids := make(map[string]struct{}, len(e.ids))
		for id := range e.ids {
			ids[id] = struct{}{}
		}
		ne := &indexEntry{key: append([]any(nil), e.key...), ids: ids}
		c.entries[i] = ne
		c.byKey[tupleKeyString(ne.key)] = ne
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

// tupleSep 用于拼接复合索引元组的 map 键。
const tupleSep = "\x1f"

// tupleKeyString 把一组索引值编码成唯一字符串。
func tupleKeyString(keys []any) string {
	if len(keys) == 1 {
		return indexKeyString(keys[0])
	}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(tupleSep)
		}
		ck, _, ok := canonicalKey(k)
		if !ok {
			return ""
		}
		b.WriteString(ck)
	}
	return b.String()
}

// compareTupleKeys 按字段顺序比较两个元组键。
func compareTupleKeys(a, b []any) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := compareIndexKeys(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

// keys 返回文档在该索引上的键元组，单字段数组按 multikey 展开。
// 缺少任一字段或值不可索引时返回 nil，该文档不进入索引。
func (ix *index) keys(doc map[string]any) [][]any {
	parts := make([][]any, 0, len(ix.fields))
	for _, field := range ix.fields {
		v, ok := getPath(doc, field)
		if !ok || v == nil {
			return nil
		}
		if arr, isArr := asArray(v); isArr {
			// 复合索引只接受标量：数组无法组成确定的元组，
			// 跳过该文档并标记索引不完整，规划时整体回退全表扫描。
			if len(ix.fields) > 1 {
				ix.incomplete = true
				return nil
			}
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
			if len(out) == 0 {
				return nil
			}
			parts = append(parts, out)
			continue
		}
		if _, _, ok := canonicalKey(v); !ok {
			return nil
		}
		parts = append(parts, []any{v})
	}

	combos := [][]any{{}}
	for _, vals := range parts {
		next := make([][]any, 0, len(combos)*len(vals))
		for _, base := range combos {
			for _, v := range vals {
				c := make([]any, len(base)+1)
				copy(c, base)
				c[len(base)] = v
				next = append(next, c)
			}
		}
		combos = next
	}
	return combos
}

func (ix *index) add(id string, doc map[string]any) {
	for _, key := range ix.keys(doc) {
		ck := tupleKeyString(key)
		if ck == "" {
			continue
		}
		e := ix.byKey[ck]
		if e == nil {
			e = &indexEntry{key: key, ids: make(map[string]struct{})}
			ix.byKey[ck] = e
			pos := sort.Search(len(ix.entries), func(i int) bool {
				return compareTupleKeys(ix.entries[i].key, key) >= 0
			})
			ix.entries = append(ix.entries, nil)
			copy(ix.entries[pos+1:], ix.entries[pos:])
			ix.entries[pos] = e
		}
		e.ids[id] = struct{}{}
	}
}

func (ix *index) remove(id string, doc map[string]any) {
	for _, key := range ix.keys(doc) {
		ck := tupleKeyString(key)
		if ck == "" {
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
	// 唯一复合索引要求所有字段都是标量，否则唯一性无法完整校验。
	if len(ix.fields) > 1 {
		for _, field := range ix.fields {
			if v, ok := getPath(doc, field); ok {
				if _, isArr := asArray(v); isArr {
					return fmt.Errorf("%w: compound unique index %q requires scalar field %q", ErrInvalid, ix.name(), field)
				}
			}
		}
	}
	for _, key := range ix.keys(doc) {
		ck := tupleKeyString(key)
		if ck == "" {
			continue
		}
		e := ix.byKey[ck]
		if e == nil {
			continue
		}
		for other := range e.ids {
			if other != id {
				return fmt.Errorf("%w: index %q conflicts on value %v", ErrDuplicateKey, ix.name(), key)
			}
		}
	}
	return nil
}

// lookup 根据一组字段条件返回候选文档 ID 集合；返回 nil 表示该索引不适用。
func (ix *index) lookup(conds map[string][]any) map[string]struct{} {
	if ix.incomplete {
		return nil
	}
	if len(ix.fields) == 1 {
		var best map[string]struct{}
		for _, cond := range conds[ix.fields[0]] {
			ids := ix.lookupSingle(cond)
			if ids == nil {
				continue
			}
			best = intersectIDs(best, ids)
			if len(best) == 0 {
				return best
			}
		}
		return best
	}
	// 复合索引只有在每个字段都有条件时才比全表扫描更划算。
	for _, field := range ix.fields {
		if len(conds[field]) == 0 {
			return nil
		}
	}
	return ix.lookupTuple(conds)
}

func intersectIDs(a, b map[string]struct{}) map[string]struct{} {
	if a == nil {
		return b
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			delete(a, id)
		}
	}
	return a
}

// lookupSingle 是单字段索引的条件求值，数组字段按 multikey 语义命中。
func (ix *index) lookupSingle(cond any) map[string]struct{} {
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
	span, ok := ix.spanFor(op, val)
	if !ok {
		return nil
	}
	out := make(map[string]struct{})
	for _, e := range ix.entries[span.lo:span.hi] {
		for id := range e.ids {
			out[id] = struct{}{}
		}
	}
	return out
}

// entrySpan 是 entries 上的左闭右开区间。
type entrySpan struct {
	lo, hi int
}

// spanFor 按首字段的范围条件二分定位条目区间。
func (ix *index) spanFor(op string, val any) (entrySpan, bool) {
	if val == nil {
		return entrySpan{}, false
	}
	if _, _, ok := canonicalKey(val); !ok {
		return entrySpan{}, false
	}
	first := func(i int) any { return ix.entries[i].key[0] }
	switch op {
	case "$eq":
		lo := sort.Search(len(ix.entries), func(i int) bool { return compareIndexKeys(first(i), val) >= 0 })
		hi := sort.Search(len(ix.entries), func(i int) bool { return compareIndexKeys(first(i), val) > 0 })
		return entrySpan{lo, hi}, true
	case "$gt":
		lo := sort.Search(len(ix.entries), func(i int) bool { return compareIndexKeys(first(i), val) > 0 })
		return entrySpan{lo, len(ix.entries)}, true
	case "$gte":
		lo := sort.Search(len(ix.entries), func(i int) bool { return compareIndexKeys(first(i), val) >= 0 })
		return entrySpan{lo, len(ix.entries)}, true
	case "$lt":
		hi := sort.Search(len(ix.entries), func(i int) bool { return compareIndexKeys(first(i), val) >= 0 })
		return entrySpan{0, hi}, true
	case "$lte":
		hi := sort.Search(len(ix.entries), func(i int) bool { return compareIndexKeys(first(i), val) > 0 })
		return entrySpan{0, hi}, true
	default:
		return entrySpan{}, false
	}
}

// lookupTuple 用首字段把条目缩到区间内，再逐条校验其余字段条件。
func (ix *index) lookupTuple(conds map[string][]any) map[string]struct{} {
	var spans []entrySpan
	for _, cond := range conds[ix.fields[0]] {
		for _, one := range splitCond(cond) {
			if span, ok := ix.spanFor(one.op, one.val); ok {
				spans = append(spans, span)
			}
		}
	}
	if len(spans) == 0 {
		return nil
	}
	out := make(map[string]struct{})
	for _, span := range spans {
		for _, e := range ix.entries[span.lo:span.hi] {
			if !matchTupleEntry(e.key, ix.fields, conds) {
				continue
			}
			for id := range e.ids {
				out[id] = struct{}{}
			}
		}
	}
	return out
}

type scalarCond struct {
	op  string
	val any
}

// splitCond 把单字段条件拆成等值/范围形式，只保留可索引的部分。
func splitCond(cond any) []scalarCond {
	if ops, ok := cond.(map[string]any); ok && hasOperator(ops) {
		out := make([]scalarCond, 0, len(ops))
		for op, val := range ops {
			switch op {
			case "$eq", "$gt", "$gte", "$lt", "$lte":
				out = append(out, scalarCond{op: op, val: val})
			}
		}
		return out
	}
	return []scalarCond{{op: "$eq", val: cond}}
}

// matchTupleEntry 校验元组是否满足每个字段上的全部条件。
func matchTupleEntry(keys []any, fields []string, conds map[string][]any) bool {
	for i, field := range fields {
		for _, cond := range conds[field] {
			if !matchIndexValue(keys[i], cond) {
				return false
			}
		}
	}
	return true
}

// matchIndexValue 在单个索引值上复用查询算子语义。
func matchIndexValue(actual, cond any) bool {
	if ops, ok := cond.(map[string]any); ok && hasOperator(ops) {
		for op, val := range ops {
			if !applyOperator(op, actual, true, val) {
				return false
			}
		}
		return true
	}
	return matchEquality(actual, cond)
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
	norm, err := normalizeIndexSpec(spec)
	if err != nil {
		return nil, err
	}
	ix := newIndex(norm)
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
	specs := make([]IndexSpec, 0, len(c.indexes)+len(c.texts)+len(c.geos))
	for _, ix := range c.indexes {
		specs = append(specs, IndexSpec{Field: ix.name(), Fields: ix.fields, Unique: ix.unique})
	}
	for name := range c.texts {
		specs = append(specs, IndexSpec{Field: name, Fields: []string{name}, Type: IndexTypeText})
	}
	for field := range c.geos {
		specs = append(specs, IndexSpec{Field: field, Fields: []string{field}, Type: IndexTypeGeo})
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Field != specs[j].Field {
			return specs[i].Field < specs[j].Field
		}
		return specs[i].Type < specs[j].Type
	})
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
	for name, ix := range c.indexes {
		nc.indexes[name] = ix.clone()
	}
	for name, tx := range c.texts {
		nc.texts[name] = tx.clone()
	}
	for field, gx := range c.geos {
		nc.geos[field] = gx.clone()
	}
	return nc
}

// attachIndex 把索引定义挂到集合上，用于快照加载与 WAL 重放。
func (c *collection) attachIndex(spec IndexSpec) error {
	switch spec.Type {
	case IndexTypeText:
		if c.texts[spec.Field] == nil {
			c.texts[spec.Field] = buildTextIndex(c)
		}
	case IndexTypeGeo:
		if _, exists := c.geos[spec.Field]; !exists {
			c.geos[spec.Field] = buildGeoIndex(c, spec.Field)
		}
	default:
		if _, exists := c.indexes[spec.Field]; exists {
			return nil
		}
		ix, err := buildIndex(c, spec)
		if err != nil {
			return err
		}
		c.indexes[spec.Field] = ix
	}
	return nil
}

// indexNameInUse 判断索引名是否已被任意类型的索引占用。
func (c *collection) indexNameInUse(name string) bool {
	if _, ok := c.indexes[name]; ok {
		return true
	}
	if _, ok := c.texts[name]; ok {
		return true
	}
	_, ok := c.geos[name]
	return ok
}

// collectConditions 收集顶层与 $and 中的字段条件；$or/$not 无法安全地取候选集。
func collectConditions(filter map[string]any, out map[string][]any) {
	for key, cond := range filter {
		switch key {
		case "$and":
			list, ok := asFilterList(cond)
			if !ok {
				continue
			}
			for _, sub := range list {
				collectConditions(sub, out)
			}
		case "$or", "$not":
			// 跳过
		default:
			out[key] = append(out[key], cond)
		}
	}
}

// planCandidates 从过滤条件中挑选候选集最小的索引，返回 nil 表示走全表扫描。
func (c *collection) planCandidates(filter map[string]any) map[string]struct{} {
	if filter == nil || (len(c.indexes) == 0 && len(c.texts) == 0 && len(c.geos) == 0) {
		return nil
	}
	conds := make(map[string][]any, len(filter))
	collectConditions(filter, conds)
	if len(conds) == 0 {
		return nil
	}
	var best map[string]struct{}
	consider := func(ids map[string]struct{}) {
		if ids == nil {
			return
		}
		if best == nil || len(ids) < len(best) {
			best = ids
		}
	}
	for _, ix := range c.indexes {
		consider(ix.lookup(conds))
	}
	for _, tx := range c.texts {
		consider(tx.lookup(conds["$text"]))
	}
	for field, gx := range c.geos {
		consider(gx.lookup(conds[field]))
	}
	return best
}

// CreateIndex 创建普通二级索引；字段用逗号分隔时创建复合索引。
func (db *DB) CreateIndex(coll, field string, unique bool) error {
	return db.CreateIndexSpec(coll, IndexSpec{Field: field, Unique: unique})
}

// CreateIndexSpec 按完整定义创建索引，支持普通、复合、文本与地理索引。
func (db *DB) CreateIndexSpec(coll string, spec IndexSpec) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.checkOpen(); err != nil {
		return err
	}
	if strings.TrimSpace(coll) == "" {
		return fmt.Errorf("%w: empty collection name", ErrInvalid)
	}
	spec, err := normalizeIndexSpec(spec)
	if err != nil {
		return err
	}
	c := db.getCollection(coll)
	name := spec.Field
	if c.indexNameInUse(name) {
		switch spec.Type {
		case IndexTypeText:
			if c.texts[name] != nil {
				return nil
			}
		case IndexTypeGeo:
			if _, exists := c.geos[name]; exists {
				return nil
			}
		default:
			if ix, exists := c.indexes[name]; exists && ix.unique == spec.Unique {
				return nil
			}
		}
		return fmt.Errorf("%w: index on %q already exists with different options", ErrInvalid, name)
	}
	if err := c.attachIndex(spec); err != nil {
		return err
	}
	// 先登记内存索引再写 WAL，避免写成功后的自动压缩丢掉索引定义。
	rec := walRecord{Op: "index.create", Coll: coll, Field: name, Fields: spec.Fields, Unique: spec.Unique, Type: spec.Type}
	if err := db.appendRecord(rec); err != nil {
		delete(c.indexes, name)
		delete(c.texts, name)
		delete(c.geos, name)
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
	if !c.indexNameInUse(field) {
		return nil
	}
	if err := db.appendRecord(walRecord{Op: "index.drop", Coll: coll, Field: field}); err != nil {
		return err
	}
	delete(c.indexes, field)
	delete(c.texts, field)
	delete(c.geos, field)
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
