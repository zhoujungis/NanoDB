package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// principal 是已通过鉴权的请求身份。
type principal struct {
	role        string
	collections map[string]struct{} // nil 表示可访问全部集合
}

func (p principal) canWrite() bool { return p.role == RoleWrite || p.role == RoleAdmin }
func (p principal) isAdmin() bool  { return p.role == RoleAdmin }

func (p principal) canCollection(coll string) bool {
	if len(p.collections) == 0 || coll == "" {
		return true
	}
	_, ok := p.collections[coll]
	return ok
}

type principalKey struct{}

// principalFrom 取出中间件写入的请求身份。
func principalFrom(r *http.Request) (principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(principal)
	return p, ok
}

// requestToken 从 Authorization: Bearer 或 X-NanoDB-Token 读取凭据。
func requestToken(r *http.Request) string {
	tok := r.Header.Get("X-NanoDB-Token")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return tok
}

// authenticate 解析请求身份。未配置任何凭据时视为 admin（保持旧的免鉴权行为）。
func (s *Server) authenticate(r *http.Request) (principal, bool) {
	if s.token == "" && s.users == nil {
		return principal{role: RoleAdmin}, true
	}
	tok := requestToken(r)
	if s.token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1 {
		return principal{role: RoleAdmin}, true
	}
	if s.users != nil {
		if u, ok := s.users.Lookup(tok); ok {
			p := principal{role: u.Role}
			if len(u.Collections) > 0 {
				p.collections = make(map[string]struct{}, len(u.Collections))
				for _, c := range u.Collections {
					p.collections[c] = struct{}{}
				}
			}
			return p, true
		}
	}
	return principal{}, false
}

// allowRequest 按角色与集合白名单判断请求是否放行。
// read 只能读；write 可读写但碰不到管理接口与索引 DDL；admin 不受限制。
func allowRequest(p principal, method, path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		return true
	}
	read := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
	// SQL 接口只读，read 角色也允许调用；集合白名单在解析语句后校验。
	sqlRequest := len(parts) == 2 && parts[1] == "sql"
	if !read && !p.canWrite() && !sqlRequest {
		return false
	}
	if parts[1] == "admin" {
		return p.isAdmin()
	}
	if len(parts) >= 3 && parts[1] == "indexes" && !read {
		return p.isAdmin()
	}
	if len(parts) == 3 && parts[1] == "collections" && method == http.MethodDelete {
		return p.isAdmin()
	}
	if coll := collectionOfPath(parts); coll != "" && !p.canCollection(coll) {
		return false
	}
	return true
}

// collectionOfPath 从 /v1 之后的路径取出集合名，取不到时返回空串。
func collectionOfPath(parts []string) string {
	if len(parts) < 2 {
		return ""
	}
	switch parts[1] {
	case "aggregate", "indexes":
		if len(parts) >= 3 {
			return parts[2]
		}
		return ""
	case "sql":
		if len(parts) >= 3 {
			return parts[2]
		}
		return ""
	case "transactions":
		if len(parts) >= 5 && parts[3] == "documents" {
			return parts[4]
		}
		return ""
	case "collections", "admin", "batch":
		return ""
	default:
		return parts[1]
	}
}

// withPrincipal 把身份写入请求上下文。
func withPrincipal(r *http.Request, p principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}
