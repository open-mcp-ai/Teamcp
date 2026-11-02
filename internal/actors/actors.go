// Package actors holds the identities one Teamcp instance can speak as.
//
// A single instance may hold ANY NUMBER of Matrix identities, on the same
// homeserver or on different ones. That is not a convenience: it is what makes
// "my planner and my coder are different participants" true, because each Matrix
// account carries its OWN read receipts and sync token. The per-actor cursor that
// a hand-rolled store would need a table for is the homeserver's job already.
package actors

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"maunium.net/go/mautrix/id"

	"github.com/open-mcp-ai/teamcp/internal/store"
	"github.com/open-mcp-ai/teamcp/internal/store/matrix"
)

// ErrNoIdentities means nothing is configured yet. It is distinct from "that name
// does not exist": the first is a deployment state a human must fix, the second
// is a caller's typo.
var ErrNoIdentities = errors.New("实例还没有配置任何身份")

// Spec is one configured identity, as it appears in the config file.
//
// AccessToken is a secret. It is never logged and never returned by Describe —
// the only thing that leaves this package about a credential is the fact that it
// exists.
type Spec struct {
	Name        string `json:"name"`
	Homeserver  string `json:"homeserver"`
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

// Info is what may be shown to a caller: no secrets.
type Info struct {
	Name       string
	UserID     string
	Homeserver string
	Default    bool
}

// Registry is the set of identities this instance can act as.
type Registry struct {
	stores  map[string]store.Store
	infos   []Info
	def     string
}

// New builds a registry from specs. Exactly one spec must be named as default;
// an empty default is resolved to the single spec when there is only one.
//
// Duplicate names are refused rather than last-one-wins: two identities with the
// same name would make `as` ambiguous, and an ambiguous `as` means a message
// attributed to the wrong actor — the one failure this design exists to prevent.
func New(specs []Spec, defaultName string) (*Registry, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("至少要配置一个身份")
	}

	r := &Registry{stores: make(map[string]store.Store, len(specs))}
	for _, s := range specs {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return nil, fmt.Errorf("身份缺少 name")
		}
		if _, dup := r.stores[name]; dup {
			return nil, fmt.Errorf("身份名 %q 重复：as 会因此变得含糊", name)
		}
		if s.Homeserver == "" || s.UserID == "" || s.AccessToken == "" {
			return nil, fmt.Errorf("身份 %q 缺少 homeserver / user_id / access_token", name)
		}
		st, err := matrix.New(strings.TrimRight(s.Homeserver, "/"), s.UserID, s.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("身份 %q：%w", name, err)
		}
		r.stores[name] = st
		r.infos = append(r.infos, Info{
			Name:       name,
			UserID:     s.UserID,
			Homeserver: s.Homeserver,
		})
	}

	if defaultName == "" {
		if len(specs) != 1 {
			return nil, fmt.Errorf("配置了 %d 个身份，必须指定 default", len(specs))
		}
		defaultName = strings.TrimSpace(specs[0].Name)
	}
	if _, ok := r.stores[defaultName]; !ok {
		return nil, fmt.Errorf("default %q 不在已配置的身份里", defaultName)
	}
	r.def = defaultName

	sort.Slice(r.infos, func(i, j int) bool { return r.infos[i].Name < r.infos[j].Name })
	for i := range r.infos {
		r.infos[i].Default = r.infos[i].Name == r.def
	}
	return r, nil
}

// Single is the shorthand deployment: one identity, three flags.
func Single(name, homeserver, userID, accessToken string) (*Registry, error) {
	if name == "" {
		name = "default"
	}
	return New([]Spec{{Name: name, Homeserver: homeserver, UserID: userID, AccessToken: accessToken}}, name)
}

// fileFormat is the JSON config shape. JSON rather than YAML on purpose: it is
// one less dependency, and the error messages a caller needs are about fields,
// not about indentation.
//
//	{
//	  "default": "planner",
//	  "actors": [
//	    {"name":"planner","homeserver":"https://m.example.org",
//	     "user_id":"@teamcp_acme_a_planner:m.example.org","access_token":"..."}
//	  ]
//	}
type fileFormat struct {
	Default string `json:"default"`
	Actors  []Spec `json:"actors"`
}

// Load reads a JSON actor file.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取身份配置 %s 失败：%w", path, err)
	}
	var f fileFormat
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("解析身份配置 %s 失败：%w", path, err)
	}
	return New(f.Actors, f.Default)
}

// Default is the identity used when a call does not name one.
func (r *Registry) Default() string { return r.def }

// Describe lists configured identities without secrets, sorted by name.
func (r *Registry) Describe() []Info { return r.infos }

// Get resolves an identity name. An empty name means the default.
//
// A miss is an error listing what exists: `as` is the one argument whose typo
// would silently post as the wrong participant if it fell back to the default.
func (r *Registry) Get(name string) (store.Store, string, error) {
	if r == nil || len(r.stores) == 0 {
		return nil, "", ErrNoIdentities
	}
	if name == "" {
		name = r.def
	}
	st, ok := r.stores[name]
	if !ok {
		return nil, name, fmt.Errorf("没有名为 %q 的身份（可用：%s）", name, strings.Join(r.Names(), ", "))
	}
	return st, name, nil
}

// PrincipalOf 回答"这个名字或 MXID 属于哪个 principal"。
//
// 它是 command 策略的判定基础：跨 principal 下命令必须被拒，而"跨"是按
// principal 算的，不是按 MXID、更不是按 agent 名。
func (r *Registry) PrincipalOf(nameOrMXID string) (string, bool) {
	if r == nil {
		return "", false
	}
	// 先按本实例配置的短名找：`--to coder` 是最自然的写法。
	for _, in := range r.infos {
		if in.Name == nameOrMXID {
			if a, err := matrix.ParseUserID(id.UserID(in.UserID)); err == nil {
				return a.Principal, true
			}
			return "", false
		}
	}
	// 再按 MXID 找：允许指向本实例之外、但仍在命名空间内的身份。
	if a, err := matrix.ParseUserID(id.UserID(nameOrMXID)); err == nil {
		return a.Principal, true
	}
	return "", false
}

// Names lists identity names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.stores))
	for name := range r.stores {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
