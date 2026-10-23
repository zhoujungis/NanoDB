package engine

import (
	"strings"
	"unicode"
)

// textIndex 是覆盖文档中所有字符串字段的倒排索引。
// 键是分词后的词元，值是包含该词元的文档 ID 集合。
type textIndex struct {
	postings map[string]map[string]struct{}
}

func newTextIndex() *textIndex {
	return &textIndex{postings: make(map[string]map[string]struct{})}
}

func (t *textIndex) clone() *textIndex {
	c := newTextIndex()
	for token, ids := range t.postings {
		set := make(map[string]struct{}, len(ids))
		for id := range ids {
			set[id] = struct{}{}
		}
		c.postings[token] = set
	}
	return c
}

func (t *textIndex) add(id string, doc map[string]any) {
	for _, token := range docTextTokens(doc, nil) {
		ids := t.postings[token]
		if ids == nil {
			ids = make(map[string]struct{})
			t.postings[token] = ids
		}
		ids[id] = struct{}{}
	}
}

func (t *textIndex) remove(id string, doc map[string]any) {
	for _, token := range docTextTokens(doc, nil) {
		ids := t.postings[token]
		if ids == nil {
			continue
		}
		delete(ids, id)
		if len(ids) == 0 {
			delete(t.postings, token)
		}
	}
}

// lookup 返回同时包含全部查询词元的候选文档集合。
func (t *textIndex) lookup(conds []any) map[string]struct{} {
	var best map[string]struct{}
	for _, cond := range conds {
		terms, _, ok := parseTextQuery(cond)
		if !ok || len(terms) == 0 {
			return nil
		}
		var ids map[string]struct{}
		for _, term := range terms {
			set := t.postings[term]
			if set == nil {
				return map[string]struct{}{}
			}
			if ids == nil {
				ids = make(map[string]struct{}, len(set))
				for id := range set {
					ids[id] = struct{}{}
				}
				continue
			}
			ids = intersectIDs(ids, set)
			if len(ids) == 0 {
				return ids
			}
		}
		best = intersectIDs(best, ids)
		if len(best) == 0 {
			return best
		}
	}
	return best
}

// buildTextIndex 从集合现有文档构建文本索引。
func buildTextIndex(c *collection) *textIndex {
	t := newTextIndex()
	for _, id := range c.ids {
		if doc := c.docs[id]; doc != nil {
			t.add(id, doc)
		}
	}
	return t
}

// docTextTokens 收集文档（或指定路径）中所有字符串的词元，自动去重。
func docTextTokens(doc map[string]any, paths []string) []string {
	seen := make(map[string]struct{})
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for _, token := range tokenizeText(t) {
				seen[token] = struct{}{}
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	if len(paths) == 0 {
		walk(doc)
	} else {
		for _, path := range paths {
			if v, ok := getPath(doc, path); ok {
				walk(v)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for token := range seen {
		out = append(out, token)
	}
	return out
}

// tokenizeText 按字母/数字切成词元；纯汉字片段额外生成二元组，
// 这样中文查询无需分词词典也能按片段匹配。
func tokenizeText(s string) []string {
	var out []string
	var word []rune
	flush := func() {
		if len(word) == 0 {
			return
		}
		han := true
		for _, r := range word {
			if !unicode.Is(unicode.Han, r) {
				han = false
				break
			}
		}
		if han && len(word) > 1 {
			for i := 0; i < len(word)-1; i++ {
				out = append(out, string(word[i:i+2]))
			}
		} else {
			out = append(out, strings.ToLower(string(word)))
		}
		word = word[:0]
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// parseTextQuery 解析 $text 条件，返回查询词元与可选的字段路径。
func parseTextQuery(cond any) (terms []string, paths []string, ok bool) {
	var search string
	switch v := cond.(type) {
	case string:
		search = v
	case map[string]any:
		s, isStr := v["$search"].(string)
		if !isStr {
			return nil, nil, false
		}
		search = s
		switch p := v["$path"].(type) {
		case string:
			if p = strings.TrimSpace(p); p != "" {
				paths = []string{p}
			}
		case []any:
			for _, item := range p {
				if s, isStr := item.(string); isStr && strings.TrimSpace(s) != "" {
					paths = append(paths, strings.TrimSpace(s))
				}
			}
		}
	default:
		return nil, nil, false
	}
	for _, term := range tokenizeText(search) {
		terms = append(terms, term)
	}
	return terms, paths, true
}

// matchText 判断文档是否同时包含查询中的全部词元。
func matchText(doc map[string]any, cond any) bool {
	terms, paths, ok := parseTextQuery(cond)
	if !ok || len(terms) == 0 {
		return false
	}
	have := make(map[string]struct{})
	for _, token := range docTextTokens(doc, paths) {
		have[token] = struct{}{}
	}
	for _, term := range terms {
		if _, ok := have[term]; !ok {
			return false
		}
	}
	return true
}
