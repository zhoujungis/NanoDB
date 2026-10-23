package engine

import (
	"fmt"
	"strings"
)

// applyUpdate 在 doc 上应用 MongoDB 风格的更新操作符。
// 支持 $set、$unset、$inc、$push、$pull、$addToSet。
func applyUpdate(doc map[string]any, patch map[string]any) (map[string]any, error) {
	if len(patch) == 0 {
		return nil, fmt.Errorf("%w: empty patch", ErrInvalidUpdate)
	}
	// 统一容器类型并深拷贝：调用方无需再复制，库内数据也不会与入参共享。
	doc = normalizeDoc(doc)
	if doc == nil {
		doc = map[string]any{}
	}
	patch = normalizeDoc(patch)
	for op, raw := range patch {
		if !strings.HasPrefix(op, "$") {
			return nil, fmt.Errorf("%w: field %q needs an update operator", ErrInvalidUpdate, op)
		}
		fields, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s expects an object", ErrInvalidUpdate, op)
		}
		for path, val := range fields {
			if path == "" {
				return nil, fmt.Errorf("%w: empty field path", ErrInvalidUpdate)
			}
			if path == "_id" || strings.HasPrefix(path, "_id.") {
				return nil, fmt.Errorf("%w: _id is immutable", ErrInvalidUpdate)
			}
			if err := applyUpdateOp(doc, op, path, val); err != nil {
				return nil, err
			}
		}
	}
	return doc, nil
}

func applyUpdateOp(doc map[string]any, op, path string, val any) error {
	switch op {
	case "$set":
		setPath(doc, path, val)

	case "$unset":
		unsetPath(doc, path)

	case "$inc":
		delta, ok := toFloat(val)
		if !ok {
			return fmt.Errorf("%w: $inc value for %q must be a number", ErrInvalidUpdate, path)
		}
		base := 0.0
		if cur, exists := getPath(doc, path); exists {
			f, ok := toFloat(cur)
			if !ok {
				return fmt.Errorf("%w: $inc target %q is not a number", ErrInvalidUpdate, path)
			}
			base = f
		}
		setPath(doc, path, base+delta)

	case "$push":
		cur, exists := getPath(doc, path)
		if !exists {
			setPath(doc, path, []any{val})
			return nil
		}
		arr, ok := cur.([]any)
		if !ok {
			return fmt.Errorf("%w: $push target %q is not an array", ErrInvalidUpdate, path)
		}
		setPath(doc, path, append(arr, val))

	case "$addToSet":
		cur, exists := getPath(doc, path)
		if !exists {
			setPath(doc, path, []any{val})
			return nil
		}
		arr, ok := cur.([]any)
		if !ok {
			return fmt.Errorf("%w: $addToSet target %q is not an array", ErrInvalidUpdate, path)
		}
		for _, item := range arr {
			if equalValues(item, val) {
				return nil
			}
		}
		setPath(doc, path, append(arr, val))

	case "$pull":
		cur, exists := getPath(doc, path)
		if !exists {
			return nil
		}
		arr, ok := cur.([]any)
		if !ok {
			return fmt.Errorf("%w: $pull target %q is not an array", ErrInvalidUpdate, path)
		}
		kept := make([]any, 0, len(arr))
		for _, item := range arr {
			if !pullMatches(item, val) {
				kept = append(kept, item)
			}
		}
		setPath(doc, path, kept)

	default:
		return fmt.Errorf("%w: unsupported operator %q", ErrInvalidUpdate, op)
	}
	return nil
}

func pullMatches(item, cond any) bool {
	if ops, ok := cond.(map[string]any); ok && hasOperator(ops) {
		return matchField(item, true, ops)
	}
	return equalValues(item, cond)
}

func setPath(doc map[string]any, path string, val any) {
	if strings.IndexByte(path, '.') < 0 {
		doc[path] = val
		return
	}
	parts := strings.Split(path, ".")
	m := doc
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = val
}

func unsetPath(doc map[string]any, path string) {
	parts := strings.Split(path, ".")
	m := doc
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, parts[len(parts)-1])
}
