package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Query 描述一次查询。
type Query struct {
	Filter map[string]any
	Sort   []SortField
	Limit  int
	Skip   int
	Fields []string
}

// SortField 描述排序字段，Desc 为 true 时降序。
type SortField struct {
	Field string
	Desc  bool
}

// ParseSort 解析 "age:-1,name:1" 形式的排序表达式。
func ParseSort(s string) ([]SortField, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []SortField
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := SortField{}
		if i := strings.LastIndex(part, ":"); i >= 0 {
			f.Field = strings.TrimSpace(part[:i])
			switch strings.TrimSpace(part[i+1:]) {
			case "1", "asc":
			case "-1", "desc":
				f.Desc = true
			default:
				return nil, fmt.Errorf("invalid sort direction in %q", part)
			}
		} else {
			f.Field = part
		}
		if f.Field == "" {
			return nil, fmt.Errorf("empty sort field in %q", part)
		}
		out = append(out, f)
	}
	return out, nil
}

func getPath(doc map[string]any, path string) (any, bool) {
	if strings.IndexByte(path, '.') < 0 {
		v, ok := doc[path]
		return v, ok
	}
	var cur any = doc
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// filterClause 是预编译后的单个过滤条件。
type filterClause struct {
	key  string
	cond any
}

// compileFilter 把过滤条件转成切片，避免在逐文档匹配时反复分配 map 迭代器。
// 同时预编译正则条件，避免扫描时每篇文档都重新编译同一条正则。
func compileFilter(filter map[string]any) []filterClause {
	if len(filter) == 0 {
		return nil
	}
	clauses := make([]filterClause, 0, len(filter))
	for key, cond := range filter {
		clauses = append(clauses, filterClause{key: key, cond: prepareCond(cond)})
	}
	return clauses
}

// regexPattern 是预编译后的正则条件；re 为 nil 表示模式非法，直接视为不命中。
type regexPattern struct {
	re *regexp.Regexp
}

// prepareCond 递归预编译条件树中的 $regex，$and/$or/$not 内的条件同样处理。
func prepareCond(cond any) any {
	m, ok := cond.(map[string]any)
	if !ok {
		return cond
	}
	out := make(map[string]any, len(m))
	for key, val := range m {
		switch key {
		case "$regex":
			s, ok := val.(string)
			if !ok {
				out[key] = val
				continue
			}
			re, err := regexp.Compile(s)
			if err != nil {
				re = nil
			}
			out[key] = regexPattern{re: re}
		case "$and", "$or":
			arr, ok := asArray(val)
			if !ok {
				out[key] = val
				continue
			}
			list := make([]any, len(arr))
			for i, item := range arr {
				list[i] = prepareCond(item)
			}
			out[key] = list
		case "$not":
			out[key] = prepareCond(val)
		default:
			out[key] = val
		}
	}
	return out
}

func matchClauses(doc map[string]any, clauses []filterClause) bool {
	for _, cl := range clauses {
		if !matchFilterKey(doc, cl.key, cl.cond) {
			return false
		}
	}
	return true
}

func matchFilter(doc map[string]any, filter map[string]any) bool {
	for key, cond := range filter {
		if !matchFilterKey(doc, key, cond) {
			return false
		}
	}
	return true
}

func matchFilterKey(doc map[string]any, key string, cond any) bool {
	switch key {
	case "$and":
		filters, ok := asFilterList(cond)
		if !ok {
			return false
		}
		for _, f := range filters {
			if !matchFilter(doc, f) {
				return false
			}
		}
		return true
	case "$or":
		filters, ok := asFilterList(cond)
		if !ok || len(filters) == 0 {
			return false
		}
		for _, f := range filters {
			if matchFilter(doc, f) {
				return true
			}
		}
		return false
	case "$not":
		f, ok := cond.(map[string]any)
		if !ok || matchFilter(doc, f) {
			return false
		}
		return true
	case "$text":
		return matchText(doc, cond)
	default:
		actual, exists := getPath(doc, key)
		return matchField(actual, exists, cond)
	}
}

func asFilterList(v any) ([]map[string]any, bool) {
	arr, ok := asArray(v)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

func matchField(actual any, exists bool, cond any) bool {
	if ops, ok := cond.(map[string]any); ok && hasOperator(ops) {
		for op, val := range ops {
			if !applyOperator(op, actual, exists, val) {
				return false
			}
		}
		return true
	}
	if !exists {
		return cond == nil
	}
	return matchEquality(actual, cond)
}

func hasOperator(m map[string]any) bool {
	for k := range m {
		if strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}

func applyOperator(op string, actual any, exists bool, val any) bool {
	switch op {
	case "$geoWithin":
		return matchGeoWithin(actual, exists, val)
	case "$eq":
		if !exists {
			return val == nil
		}
		return matchEquality(actual, val)
	case "$ne":
		if !exists {
			return true
		}
		return !matchEquality(actual, val)
	case "$gt", "$gte", "$lt", "$lte":
		if !exists {
			return false
		}
		cmp, ok := compareValues(actual, val)
		if !ok {
			return false
		}
		switch op {
		case "$gt":
			return cmp > 0
		case "$gte":
			return cmp >= 0
		case "$lt":
			return cmp < 0
		default:
			return cmp <= 0
		}
	case "$in":
		items, ok := asArray(val)
		if !ok || !exists {
			return false
		}
		for _, item := range items {
			if matchEquality(actual, item) {
				return true
			}
		}
		return false
	case "$nin":
		items, ok := asArray(val)
		if !ok {
			return false
		}
		if !exists {
			return true
		}
		for _, item := range items {
			if matchEquality(actual, item) {
				return false
			}
		}
		return true
	case "$exists":
		want, ok := val.(bool)
		if !ok {
			return false
		}
		return exists == want
	case "$regex":
		return regexMatch(actual, exists, val)
	case "$not":
		return !matchField(actual, exists, val)
	default:
		return false
	}
}

func matchEquality(actual, want any) bool {
	if arr, ok := asArray(actual); ok {
		if _, isArr := asArray(want); !isArr {
			for _, item := range arr {
				if equalValues(item, want) {
					return true
				}
			}
		}
	}
	return equalValues(actual, want)
}

func equalValues(a, b any) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}

func compareValues(a, b any) (int, bool) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return strings.Compare(as, bs), true
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// normalize 将各类数字统一为 float64，便于结构比较。
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = normalize(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = normalize(val)
		}
		return s
	default:
		if f, ok := toFloat(t); ok {
			return f
		}
		return v
	}
}

func regexMatch(actual any, exists bool, pattern any) bool {
	var re *regexp.Regexp
	switch p := pattern.(type) {
	case regexPattern:
		re = p.re
	case string:
		compiled, err := regexp.Compile(p)
		if err != nil {
			return false
		}
		re = compiled
	default:
		return false
	}
	if re == nil || !exists {
		return false
	}
	check := func(v any) bool {
		s, ok := v.(string)
		return ok && re.MatchString(s)
	}
	if arr, ok := asArray(actual); ok {
		for _, item := range arr {
			if check(item) {
				return true
			}
		}
		return false
	}
	return check(actual)
}

func sortDocs(docs []map[string]any, fields []SortField) {
	if len(fields) == 0 || len(docs) < 2 {
		return
	}
	keys := extractSortKeys(docs, fields)
	nf := len(fields)
	idx := make([]int, len(docs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		cmp := compareSortKeys(keys, idx[a], idx[b], nf, fields)
		if cmp != 0 {
			return cmp < 0
		}
		return idx[a] < idx[b]
	})
	sorted := make([]map[string]any, len(docs))
	for i, p := range idx {
		sorted[i] = docs[p]
	}
	copy(docs, sorted)
}

type missingSortValue struct{}

// extractSortKeys 一次性抽取所有文档的排序键，按 [doc0 的字段..., doc1 的字段...] 平铺存放。
func extractSortKeys(docs []map[string]any, fields []SortField) []any {
	nf := len(fields)
	keys := make([]any, len(docs)*nf)
	for i, doc := range docs {
		base := i * nf
		for j, f := range fields {
			if v, ok := getPath(doc, f.Field); ok {
				keys[base+j] = v
			} else {
				keys[base+j] = missingSortValue{}
			}
		}
	}
	return keys
}

// compareSortKeys 比较两个文档的预抽取排序键：负数表示 i 排在 j 之前。
func compareSortKeys(keys []any, i, j, nf int, fields []SortField) int {
	ibase, jbase := i*nf, j*nf
	for k := 0; k < nf; k++ {
		av, bv := keys[ibase+k], keys[jbase+k]
		_, aMissing := av.(missingSortValue)
		_, bMissing := bv.(missingSortValue)
		if aMissing && bMissing {
			continue
		}
		if aMissing {
			return 1
		}
		if bMissing {
			return -1
		}
		cmp, ok := compareValues(av, bv)
		if !ok {
			cmp = strings.Compare(fmt.Sprint(normalize(av)), fmt.Sprint(normalize(bv)))
		}
		if cmp == 0 {
			continue
		}
		if fields[k].Desc {
			return -cmp
		}
		return cmp
	}
	return 0
}

// sortIndexLess 在排序键之外用原始下标决胜，保证与全量排序一致。
func sortIndexLess(keys []any, i, j, nf int, fields []SortField) bool {
	cmp := compareSortKeys(keys, i, j, nf, fields)
	if cmp != 0 {
		return cmp < 0
	}
	return i < j
}

// docMaxHeap 是排序键意义下的最大堆，堆顶为当前前 k 条中最靠后的一条。
type docMaxHeap struct {
	keys   []any
	nf     int
	fields []SortField
	idx    []int
}

func (h *docMaxHeap) less(a, b int) bool {
	return sortIndexLess(h.keys, h.idx[a], h.idx[b], h.nf, h.fields)
}

func (h *docMaxHeap) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h.less(p, i) {
			return
		}
		h.idx[p], h.idx[i] = h.idx[i], h.idx[p]
		i = p
	}
}

func (h *docMaxHeap) down(i int) {
	n := len(h.idx)
	for {
		left := 2*i + 1
		if left >= n {
			return
		}
		largest := left
		if right := left + 1; right < n && h.less(left, right) {
			largest = right
		}
		if !h.less(i, largest) {
			return
		}
		h.idx[i], h.idx[largest] = h.idx[largest], h.idx[i]
		i = largest
	}
}

// topNSortDocs 用大小为 k 的最大堆选出排序后的前 k 条，再对这 k 条排序。
// k >= len(docs) 时退化为全量排序。
func topNSortDocs(docs []map[string]any, fields []SortField, k int) []map[string]any {
	if k <= 0 || k >= len(docs) {
		sortDocs(docs, fields)
		return docs
	}
	h := &docMaxHeap{
		keys:   extractSortKeys(docs, fields),
		nf:     len(fields),
		fields: fields,
		idx:    make([]int, 0, k),
	}
	for i := range docs {
		if len(h.idx) < k {
			h.idx = append(h.idx, i)
			h.up(len(h.idx) - 1)
			continue
		}
		if sortIndexLess(h.keys, i, h.idx[0], h.nf, h.fields) {
			h.idx[0] = i
			h.down(0)
		}
	}
	sort.Slice(h.idx, func(a, b int) bool {
		return sortIndexLess(h.keys, h.idx[a], h.idx[b], h.nf, h.fields)
	})
	out := make([]map[string]any, len(h.idx))
	for j, i := range h.idx {
		out[j] = docs[i]
	}
	return out
}

// copyProjectedDoc 在拷贝的同时完成投影，避免先整体深拷贝再裁剪。
func copyProjectedDoc(doc map[string]any, fields []string) map[string]any {
	out := make(map[string]any, len(fields)+1)
	if v, ok := doc["_id"]; ok {
		out["_id"] = deepCopy(v)
	}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || f == "_id" {
			continue
		}
		if v, ok := getPath(doc, f); ok {
			setPath(out, f, deepCopy(v))
		}
	}
	return out
}
