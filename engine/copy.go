package engine

import "reflect"

// normalizeDoc 深拷贝文档，并把常见的 Go 容器类型（如 []string、map[string]string）
// 统一成 JSON 解码后的表示（[]any、map[string]any）。
//
// 这样查询、排序、索引与深拷贝语义在 Go API 和 JSON 入口之间保持一致；
// 同时保证库内数据不与调用方共享。
func normalizeDoc(doc map[string]any) map[string]any {
	if doc == nil {
		return nil
	}
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = normalizeValue(v)
	}
	return out
}

func normalizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeValue(val)
		}
		return out
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String || rv.IsNil() {
			return v
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[iter.Key().String()] = normalizeValue(iter.Value().Interface())
		}
		return out
	case reflect.Slice:
		// []byte 保留原类型，避免改变 JSON 的 base64 编码语义。
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		if rv.IsNil() {
			return []any(nil)
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = normalizeValue(rv.Index(i).Interface())
		}
		return out
	case reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = normalizeValue(rv.Index(i).Interface())
		}
		return out
	default:
		return v
	}
}

// asArray 把任意切片/数组转换成 []any；非数组类型返回 false。
// 让 Go 调用方传入的 []string、[]int 等也能按 JSON 数组语义参与查询。
func asArray(v any) ([]any, bool) {
	if arr, ok := v.([]any); ok {
		return arr, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// deepCopy 复制文档使用的 JSON 类数据，避免调用方直接改到库内数据。
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = deepCopy(val)
		}
		return s
	default:
		return v
	}
}

func deepCopyDoc(doc map[string]any) map[string]any {
	if doc == nil {
		return nil
	}
	return deepCopy(doc).(map[string]any)
}
