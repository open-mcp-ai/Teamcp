package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/open-mcp-ai/teamcp/internal/store"
)

// Manager owns the identity set and is the only thing that may change it.
//
// THE SPLIT IT ENFORCES IS THE POINT: a human configures identities (WebUI, CLI),
// an AI only NAMES one. The DSL has no verb that adds an identity, because adding
// one grows the capability surface — the same boundary Termcp draws with its
// review gate. An AI that could mint an identity could speak as any principal.
//
// Manager implements the three methods the tool layer needs (Get/Describe/
// Default), reading the CURRENT registry on every call. That is what makes a
// console edit take effect without a restart.
type Manager struct {
	path string // empty = in-memory only (tests, one-shot CLI)

	mu    sync.RWMutex
	specs []Spec // authoritative, INCLUDING tokens; never leaves this package
	def   string
	cur   *Registry
}

// Open loads the actor file, or starts empty when it does not exist yet.
//
// Starting empty is deliberate: the first thing a human does on a fresh install
// is open the console and add an account. Refusing to boot until a file exists
// would force them to hand-write JSON first.
func Open(path string) (*Manager, error) {
	m := &Manager{path: path, cur: emptyRegistry()}
	if path == "" {
		return m, nil
	}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取身份配置 %s 失败：%w", path, err)
	}

	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析身份配置 %s 失败：%w", path, err)
	}
	if len(f.Actors) == 0 {
		return m, nil
	}
	reg, err := New(f.Actors, f.Default)
	if err != nil {
		return nil, fmt.Errorf("身份配置 %s 无效：%w", path, err)
	}
	m.specs, m.def, m.cur = f.Actors, f.Default, reg
	return m, nil
}

func emptyRegistry() *Registry {
	return &Registry{stores: map[string]store.Store{}}
}

// Get, Describe and Default read the current registry, so a configuration change
// is visible to the next call without a restart.
func (m *Manager) Get(name string) (store.Store, string, error) {
	m.mu.RLock()
	cur := m.cur
	m.mu.RUnlock()
	return cur.Get(name)
}

func (m *Manager) Describe() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur.Describe()
}

// PrincipalOf 读当前注册表，所以 WebUI 新加的身份立刻参与 command 策略判定。
func (m *Manager) PrincipalOf(nameOrMXID string) (string, bool) {
	m.mu.RLock()
	cur := m.cur
	m.mu.RUnlock()
	return cur.PrincipalOf(nameOrMXID)
}

func (m *Manager) Default() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur.def
}

// Add validates, persists, then swaps. A spec that fails validation changes
// nothing — a half-written identity set is worse than a rejected one.
//
// Validation goes through the same path startup uses (matrix.New parses the MXID
// and enforces the namespace), so a console form cannot store something the
// server would refuse to load on the next boot.
//
// Order matters: persist FIRST, swap second. If the write fails, memory still
// matches disk, and the caller learns the write failed instead of seeing an
// identity that vanishes on restart.
func (m *Manager) Add(s Spec, makeDefault bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	specs := append([]Spec(nil), m.specs...)
	replaced := false
	for i := range specs {
		if specs[i].Name == s.Name {
			specs[i] = s
			replaced = true
			break
		}
	}
	if !replaced {
		specs = append(specs, s)
	}

	def := m.def
	if makeDefault || def == "" {
		def = s.Name
	}

	reg, err := New(specs, def)
	if err != nil {
		return err
	}
	if err := m.persist(specs, def); err != nil {
		return err
	}
	m.specs, m.def, m.cur = specs, def, reg
	return nil
}

// Remove deletes an identity. Removing the last one leaves an empty set (the
// server keeps running: the guide still answers); removing the default promotes
// whoever sorts first.
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var kept []Spec
	found := false
	for _, s := range m.specs {
		if s.Name == name {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	if !found {
		return fmt.Errorf("没有名为 %q 的身份", name)
	}
	if len(kept) == 0 {
		if err := m.persist(nil, ""); err != nil {
			return err
		}
		m.specs, m.def, m.cur = nil, "", emptyRegistry()
		return nil
	}

	def := m.def
	if def == name {
		sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
		def = kept[0].Name
	}
	reg, err := New(kept, def)
	if err != nil {
		return err
	}
	if err := m.persist(kept, def); err != nil {
		return err
	}
	m.specs, m.def, m.cur = kept, def, reg
	return nil
}

// SetDefault changes which identity a call uses when it names none.
func (m *Manager) SetDefault(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	reg, err := New(m.specs, name)
	if err != nil {
		return err
	}
	if err := m.persist(m.specs, name); err != nil {
		return err
	}
	m.def, m.cur = name, reg
	return nil
}

// Test proves an identity works RIGHT NOW by calling the homeserver's whoami.
//
// It is what a console "测试" button calls, so a wrong token is caught in the
// form rather than at the AI's first message. A test never mutates the set.
func (m *Manager) Test(ctx context.Context, name string) (store.Actor, error) {
	st, _, err := m.Get(name)
	if err != nil {
		return store.Actor{}, err
	}
	return st.Whoami(ctx)
}

// persist writes the file atomically: temp file in the same directory, fsync,
// then rename.
//
// rename() is the one atomic primitive a plain file gets, so a crash leaves
// either the old set or the new one — never a half-written one. Permissions are
// 0600 because this file holds access tokens.
func (m *Manager) persist(specs []Spec, def string) error {
	if m.path == "" {
		return nil
	}
	body, err := json.MarshalIndent(fileFormat{Default: def, Actors: specs}, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	tmp, err := os.CreateTemp(dir, ".actors-*.tmp")
	if err != nil {
		return fmt.Errorf("写入身份配置失败：%w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, m.path)
}
