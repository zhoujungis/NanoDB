package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 角色由低到高：read < write < admin。
const (
	RoleRead  = "read"
	RoleWrite = "write"
	RoleAdmin = "admin"
)

// 用户管理相关错误。
var (
	ErrUserNotFound = errors.New("nanodb: user not found")
	ErrUserExists   = errors.New("nanodb: user already exists")
	ErrInvalidRole  = errors.New("nanodb: invalid role")
	ErrEmptyName    = errors.New("nanodb: empty user name")
)

// User 是一个 HTTP 访问账号，Token 作为 Bearer 凭据。
// Collections 为空表示可访问全部集合。
type User struct {
	Name        string   `json:"name"`
	Token       string   `json:"token"`
	Role        string   `json:"role"`
	Collections []string `json:"collections,omitempty"`
}

func (u User) allowsCollection(coll string) bool {
	if len(u.Collections) == 0 || coll == "" {
		return true
	}
	for _, c := range u.Collections {
		if c == coll {
			return true
		}
	}
	return false
}

// UserStore 把用户账号持久化到 JSON 文件，并支持按 token 反查。
type UserStore struct {
	path    string
	mu      sync.RWMutex
	users   map[string]User
	modTime int64
}

// OpenUserStore 从文件加载用户；文件不存在时返回空存储。
func OpenUserStore(path string) (*UserStore, error) {
	s := &UserStore{path: path, users: make(map[string]User)}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path 返回用户文件路径。
func (s *UserStore) Path() string { return s.path }

// reload 读取文件并替换内存数据；文件不存在视为空。
func (s *UserStore) reload() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("nanodb: stat users file: %w", err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("nanodb: read users file: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var file struct {
		Users []User `json:"users"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("nanodb: parse users file: %w", err)
	}
	users := make(map[string]User, len(file.Users))
	for _, u := range file.Users {
		if u.Name == "" {
			continue
		}
		if u.Role == "" {
			u.Role = RoleWrite
		}
		users[u.Name] = u
	}
	s.mu.Lock()
	s.users = users
	s.modTime = info.ModTime().UnixNano()
	s.mu.Unlock()
	return nil
}

// refresh 在文件被外部修改（例如 CLI 新增用户）后自动重新加载。
func (s *UserStore) refresh() {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.mu.RLock()
	changed := info.ModTime().UnixNano() != s.modTime
	s.mu.RUnlock()
	if changed {
		_ = s.reload()
	}
}

func (s *UserStore) saveLocked() error {
	list := make([]User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(struct {
		Users []User `json:"users"`
	}{Users: list}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("nanodb: create users dir: %w", err)
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("nanodb: write users file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("nanodb: replace users file: %w", err)
	}
	if info, err := os.Stat(s.path); err == nil {
		s.modTime = info.ModTime().UnixNano()
	}
	return nil
}

// Lookup 按 token 查找用户，使用常量时间比较避免时序泄漏。
func (s *UserStore) Lookup(token string) (User, bool) {
	if token == "" {
		return User{}, false
	}
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if subtle.ConstantTimeCompare([]byte(u.Token), []byte(token)) == 1 {
			return u, true
		}
	}
	return User{}, false
}

// Get 按名字查找用户。
func (s *UserStore) Get(name string) (User, bool) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[name]
	return u, ok
}

// List 返回按名字排序的用户列表（包含 token，供 CLI 展示）。
func (s *UserStore) List() []User {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Add 新建用户并生成随机 token；collections 为空表示可访问全部集合。
func (s *UserStore) Add(name, role string, collections []string) (User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return User{}, ErrEmptyName
	}
	if err := ValidateRole(role); err != nil {
		return User{}, err
	}
	token, err := newUserToken()
	if err != nil {
		return User{}, err
	}
	colls := normalizeCollections(collections)

	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.users[name]; exists {
		return User{}, fmt.Errorf("%w: %s", ErrUserExists, name)
	}
	s.users[name] = User{Name: name, Token: token, Role: role, Collections: colls}
	if err := s.saveLocked(); err != nil {
		delete(s.users, name)
		return User{}, err
	}
	return s.users[name], nil
}

// Remove 删除用户。
func (s *UserStore) Remove(name string) error {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	u, exists := s.users[name]
	if !exists {
		return fmt.Errorf("%w: %s", ErrUserNotFound, name)
	}
	delete(s.users, name)
	if err := s.saveLocked(); err != nil {
		s.users[name] = u
		return err
	}
	return nil
}

// ValidateRole 校验角色名称。
func ValidateRole(role string) error {
	switch role {
	case RoleRead, RoleWrite, RoleAdmin:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
}

func normalizeCollections(collections []string) []string {
	out := make([]string, 0, len(collections))
	seen := make(map[string]struct{}, len(collections))
	for _, c := range collections {
		if c = strings.TrimSpace(c); c != "" {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// newUserToken 生成 32 字节随机 token 的十六进制表示。
func newUserToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("nanodb: generate token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
