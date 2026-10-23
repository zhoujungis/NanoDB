package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Aggregate 在集合上执行聚合管道，返回处理后的文档副本。
// 支持的阶段：$match、$project、$group、$sort、$skip、$limit、$unwind、$count。
func (db *DB) Aggregate(coll string, pipeline []map[string]any) ([]map[string]any, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrClosed
	}
	c := db.colls[coll]
	if c == nil {
		return []map[string]any{}, nil
	}
	docs, err := runPipeline(c, pipeline)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(docs))
	for i, doc := range docs {
		out[i] = deepCopyDoc(doc)
	}
	return out, nil
}

func runPipeline(c *collection, pipeline []map[string]any) ([]map[string]any, error) {
	docs := make([]map[string]any, 0, len(c.ids))
	for _, id := range c.ids {
		if doc := c.docs[id]; doc != nil {
			docs = append(docs, doc)
		}
	}
	for i, stage := range pipeline {
		if len(stage) != 1 {
			return nil, fmt.Errorf("%w: stage %d must contain exactly one operator", ErrInvalid, i)
		}
		for op, spec := range stage {
			var err error
			docs, err = applyStage(docs, op, spec)
			if err != nil {
				return nil, fmt.Errorf("%w: stage %d (%s): %v", ErrInvalid, i, op, err)
			}
		}
	}
	return docs, nil
}

func applyStage(docs []map[string]any, op string, spec any) ([]map[string]any, error) {
	switch op {
	case "$match":
		filter, ok := spec.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$match requires an object")
		}
		clauses := compileFilter(filter)
		out := make([]map[string]any, 0, len(docs))
		for _, doc := range docs {
			if matchClauses(doc, clauses) {
				out = append(out, doc)
			}
		}
		return out, nil

	case "$limit":
		n, ok := stageInt(spec)
		if !ok || n < 0 {
			return nil, fmt.Errorf("$limit requires a non-negative integer")
		}
		if n < len(docs) {
			return docs[:n], nil
		}
		return docs, nil

	case "$skip":
		n, ok := stageInt(spec)
		if !ok || n < 0 {
			return nil, fmt.Errorf("$skip requires a non-negative integer")
		}
		if n >= len(docs) {
			return nil, nil
		}
		return docs[n:], nil

	case "$sort":
		fields, err := parseSortSpec(spec)
		if err != nil {
			return nil, err
		}
		sortDocs(docs, fields)
		return docs, nil

	case "$project":
		return projectStage(docs, spec)

	case "$group":
		return groupStage(docs, spec)

	case "$unwind":
		return unwindStage(docs, spec)

	case "$count":
		name, ok := spec.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("$count requires a field name")
		}
		if len(docs) == 0 {
			return []map[string]any{}, nil
		}
		return []map[string]any{{name: float64(len(docs))}}, nil

	default:
		return nil, fmt.Errorf("unsupported stage %q", op)
	}
}

// stageInt 接受 JSON 数字或 Go 整数，拒绝小数与负数。
func stageInt(spec any) (int, bool) {
	f, ok := toFloat(spec)
	if !ok || f < 0 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// parseSortSpec 支持 "age:-1"、{"age":-1} 与 [{"age":-1},{"name":1}] 三种写法。
// map 形式没有顺序，按字段名排序以保证结果稳定。
func parseSortSpec(spec any) ([]SortField, error) {
	switch v := spec.(type) {
	case string:
		return ParseSort(v)
	case map[string]any:
		fields := make([]SortField, 0, len(v))
		for f, dir := range v {
			d, ok := toFloat(dir)
			if !ok || (d != 1 && d != -1) {
				return nil, fmt.Errorf("sort direction for %q must be 1 or -1", f)
			}
			fields = append(fields, SortField{Field: f, Desc: d < 0})
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Field < fields[j].Field })
		return fields, nil
	case []any:
		var out []SortField
		for _, item := range v {
			fs, err := parseSortSpec(item)
			if err != nil {
				return nil, err
			}
			out = append(out, fs...)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("$sort requires a field or object")
	}
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case nil:
		return false
	default:
		if f, ok := toFloat(t); ok {
			return f != 0
		}
		return true
	}
}

func projectStage(docs []map[string]any, spec any) ([]map[string]any, error) {
	m, ok := spec.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, fmt.Errorf("$project requires a non-empty object")
	}
	include := false
	for _, v := range m {
		if truthy(v) {
			include = true
			break
		}
	}

	out := make([]map[string]any, 0, len(docs))
	if include {
		fields := make([]string, 0, len(m))
		for f, v := range m {
			if truthy(v) {
				fields = append(fields, f)
			}
		}
		sort.Strings(fields)
		for _, doc := range docs {
			out = append(out, copyProjectedDoc(doc, fields))
		}
		return out, nil
	}

	for _, doc := range docs {
		cp := deepCopyDoc(doc)
		for f, v := range m {
			if truthy(v) {
				continue
			}
			deletePath(cp, f)
		}
		out = append(out, cp)
	}
	return out, nil
}

func deletePath(doc map[string]any, path string) {
	parts := strings.Split(path, ".")
	cur := doc
	for i, p := range parts {
		if i == len(parts)-1 {
			delete(cur, p)
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
}

func unwindStage(docs []map[string]any, spec any) ([]map[string]any, error) {
	var path string
	preserve := false
	switch v := spec.(type) {
	case string:
		path = v
	case map[string]any:
		p, _ := v["path"].(string)
		path = p
		if b, ok := v["preserveNullAndEmptyArrays"].(bool); ok {
			preserve = b
		}
	default:
		return nil, fmt.Errorf("$unwind requires a field path")
	}
	path = strings.TrimPrefix(strings.TrimSpace(path), "$")
	if path == "" {
		return nil, fmt.Errorf("$unwind requires a field path")
	}

	out := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		v, ok := getPath(doc, path)
		arr, isArr := asArray(v)
		if !ok || !isArr || len(arr) == 0 {
			if preserve {
				out = append(out, doc)
			}
			continue
		}
		for _, item := range arr {
			cp := deepCopyDoc(doc)
			setPath(cp, path, deepCopy(item))
			out = append(out, cp)
		}
	}
	return out, nil
}

// accState 是单个分组内某个累加器的中间状态。
type accState struct {
	op    string
	sum   float64
	nums  int
	count int
	set   bool
	value any
	list  []any
}

func groupStage(docs []map[string]any, spec any) ([]map[string]any, error) {
	m, ok := spec.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, fmt.Errorf("$group requires a non-empty object")
	}
	idExpr, hasID := m["_id"]
	if !hasID {
		idExpr = nil
	}
	// 先校验累加器定义，避免运行到一半才报错。
	for field, accSpec := range m {
		if field == "_id" {
			continue
		}
		if _, _, err := parseAccumulator(accSpec); err != nil {
			return nil, fmt.Errorf("accumulator %q: %w", field, err)
		}
	}

	type group struct {
		key  any
		accs map[string]*accState
	}
	order := make([]string, 0)
	groups := make(map[string]*group)

	for _, doc := range docs {
		key := evalAggExpr(doc, idExpr)
		enc, err := encodeGroupKey(key)
		if err != nil {
			return nil, fmt.Errorf("encode group key: %w", err)
		}
		g := groups[enc]
		if g == nil {
			g = &group{key: key, accs: make(map[string]*accState, len(m)-1)}
			groups[enc] = g
			order = append(order, enc)
		}
		for field, accSpec := range m {
			if field == "_id" {
				continue
			}
			op, arg, _ := parseAccumulator(accSpec)
			st := g.accs[field]
			if st == nil {
				st = &accState{op: op}
				g.accs[field] = st
			}
			accumulate(st, arg, doc)
		}
	}

	out := make([]map[string]any, 0, len(order))
	for _, enc := range order {
		g := groups[enc]
		row := map[string]any{"_id": deepCopy(g.key)}
		for field, accSpec := range m {
			if field == "_id" {
				continue
			}
			op, _, _ := parseAccumulator(accSpec)
			row[field] = finalizeAcc(g.accs[field], op)
		}
		out = append(out, row)
	}
	return out, nil
}

func parseAccumulator(accSpec any) (string, any, error) {
	m, ok := accSpec.(map[string]any)
	if !ok || len(m) != 1 {
		return "", nil, fmt.Errorf("accumulator must be a single {$op: value}")
	}
	for op, arg := range m {
		switch op {
		case "$sum", "$avg", "$min", "$max", "$first", "$last", "$push", "$count":
			return op, arg, nil
		default:
			return "", nil, fmt.Errorf("unsupported accumulator %q", op)
		}
	}
	return "", nil, nil
}

func accumulate(st *accState, arg any, doc map[string]any) {
	val := evalAggExpr(doc, arg)
	st.count++
	switch st.op {
	case "$sum":
		if f, ok := toFloat(val); ok {
			st.sum += f
		}
	case "$avg":
		if f, ok := toFloat(val); ok {
			st.sum += f
			st.nums++
		}
	case "$min":
		if !st.set {
			st.value = val
			st.set = true
			return
		}
		if cmp, ok := compareValues(val, st.value); ok && cmp < 0 {
			st.value = val
		}
	case "$max":
		if !st.set {
			st.value = val
			st.set = true
			return
		}
		if cmp, ok := compareValues(val, st.value); ok && cmp > 0 {
			st.value = val
		}
	case "$first":
		if st.count == 1 {
			st.value = val
		}
	case "$last":
		st.value = val
	case "$push":
		st.list = append(st.list, val)
	case "$count":
		// 只统计条数
	}
}

func finalizeAcc(st *accState, op string) any {
	if st == nil {
		return nil
	}
	switch op {
	case "$sum":
		return st.sum
	case "$avg":
		if st.nums == 0 {
			return float64(0)
		}
		return st.sum / float64(st.nums)
	case "$min", "$max", "$first", "$last":
		return st.value
	case "$push":
		if st.list == nil {
			return []any{}
		}
		return st.list
	case "$count":
		return float64(st.count)
	default:
		return nil
	}
}

// evalAggExpr 求值聚合表达式：$path 取字段，其他值为字面量，对象与数组递归求值。
func evalAggExpr(doc map[string]any, expr any) any {
	switch v := expr.(type) {
	case string:
		if strings.HasPrefix(v, "$") {
			if val, ok := getPath(doc, strings.TrimPrefix(v, "$")); ok {
				return val
			}
			return nil
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, sub := range v {
			out[k] = evalAggExpr(doc, sub)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, sub := range v {
			out[i] = evalAggExpr(doc, sub)
		}
		return out
	default:
		return v
	}
}

// encodeGroupKey 把分组键编码为稳定的字符串；normalize 统一数字类型，
// json.Marshal 对 map 键排序，保证同一逻辑键得到同一编码。
func encodeGroupKey(key any) (string, error) {
	data, err := json.Marshal(normalize(key))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
