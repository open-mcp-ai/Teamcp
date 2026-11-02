// Package tool is the single MCP entry of Teamcp: one tool, named `team`,
// whose whole capability lives in the `cmd` string rather than in the schema.
//
// This file is the ARGUMENT TABLE. It is the one place a tool argument is
// declared, and everything that publishes an argument reads it: the JSON Schema
// handed to the SDK, the parser below, and (later) docs/api.md. Three hand-kept
// copies is how a published switch ends up missing from the reference — that is
// not hypothetical, it is the drift the argument table exists to prevent.
package tool

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Kind is the value type an argument carries.
type Kind int

const (
	KindString Kind = iota
	KindNumber
	KindBool
	KindStringArray
	KindStringMap
)

// Decl is one argument of the team tool.
type Decl struct {
	// Name is the wire name.
	Name string
	// Kind is the value type, and therefore which JSON shapes are accepted.
	Kind Kind
	// Desc is the sentence a caller reads in the schema. It is published
	// verbatim, so it must stand alone: clients differ on whether they render
	// the `default` field, which is why the default is repeated in prose below.
	Desc string
	// Default is the value used when the argument is ABSENT. Present-and-wrong
	// is not absorbed by this — that is a 400 (see Parse).
	Default any
	// Enum, when non-empty, closes the set of accepted strings.
	Enum []string
	// Min, when non-nil, is the lowest accepted number. There is no Max on
	// purpose: every upper bound this table could carry is a size or a duration
	// the CALLER pays for, and refusing it would make this server overrule the
	// only party that can judge the trade.
	Min *float64
}

func f64(v float64) *float64 { return &v }

// Decls is THE table. Order is publication order: the schema renders top to
// bottom, so a reader scanning it meets the arguments in a stable sequence.
var Decls = []Decl{
	{
		Name:    "cmd",
		Kind:    KindString,
		Desc:    "终端式 DSL。空 = 引导（我在哪、能做什么、这里有什么）。",
		Default: "",
	},
	{
		// `as` 是参数而不是指令，因为它作用于整条 cmd 的每一条语句，而不是某一条。
		// 做成 `as planner` 这样的状态型指令会引入跨语句的隐式状态，而我们整个设计
		// 是无状态 + 显式参数的。作为参数，它天然与 `;` 组合对齐：这一批就是我。
		Name:    "as",
		Kind:    KindString,
		Desc:    "以哪个身份说话/读。省略 = 默认身份。一个实例可配任意多个 Matrix 身份，同一或不同 homeserver 都行。",
		Default: "",
	},
	{
		Name:    "timeout",
		Kind:    KindNumber,
		Desc:    "等新消息的秒数；0 = 不等。只被 wait 消费。建议 45 秒以内，不设硬墙。默认 0。",
		Default: 0.0,
		Min:     f64(0),
	},
	{
		Name:    "limit",
		Kind:    KindNumber,
		Desc:    "每个 slot 最多几条消息；0 = 不限。默认 20。",
		Default: 20.0,
		Min:     f64(0),
	},
	{
		Name:    "total_limit",
		Kind:    KindNumber,
		Desc:    "整条回复的消息总数上限，超出的 slot 被裁掉并说明裁了哪些。默认 100。",
		Default: 100.0,
		Min:     f64(1),
	},
	{
		Name:    "learn_mode",
		Kind:    KindBool,
		Desc:    "每条回复在 slot 末尾附一行 Usage。默认 false。",
		Default: false,
	},
	{
		Name:    "structured_content",
		Kind:    KindBool,
		Desc:    "额外给 JSON 投影（structuredContent）。默认 false：两个投影内容相同，开启等于每轮回复付两遍钱。",
		Default: false,
	},
	{
		Name:    "content",
		Kind:    KindBool,
		Desc:    "给渲染后的文本投影。默认 true。两个投影都拒绝是 400。",
		Default: true,
	},
	{
		Name:    "args",
		Kind:    KindStringArray,
		Desc:    "位置值 $1、$2……（1 基）。每个值都必须被 cmd 引用；没被引用的值会被拒绝，因为被丢掉的参数在回复里看不见。",
		Default: nil,
	},
	{
		Name:    "vars",
		Kind:    KindStringMap,
		Desc:    "命名值 $NAME。会原样写进服务器日志。",
		Default: nil,
	},
	{
		Name:    "secrets",
		Kind:    KindStringMap,
		Desc:    "命名值 $NAME，永不写进日志。放凭据。",
		Default: nil,
	},
}

func declFor(name string) (Decl, bool) {
	for _, d := range Decls {
		if d.Name == name {
			return d, true
		}
	}
	return Decl{}, false
}

// Schema renders the tool's JSON Schema from Decls.
//
// additionalProperties is false so a client can refuse a misspelled argument
// BEFORE the call; Parse enforces the same rule for the REST entrance, which
// never reaches the SDK's validator. One declaration, two enforcement points.
func Schema() map[string]any {
	props := make(map[string]any, len(Decls))
	for _, d := range Decls {
		props[d.Name] = propertySchema(d)
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
}

func propertySchema(d Decl) map[string]any {
	out := map[string]any{"description": d.Desc}
	switch d.Kind {
	case KindString:
		out["type"] = "string"
		if len(d.Enum) > 0 {
			out["enum"] = d.Enum
		}
	case KindNumber:
		out["type"] = "number"
		if d.Min != nil {
			out["minimum"] = *d.Min
		}
	case KindBool:
		out["type"] = "boolean"
	case KindStringArray:
		out["type"] = "array"
		out["items"] = map[string]any{"type": "string"}
	case KindStringMap:
		out["type"] = "object"
		out["additionalProperties"] = map[string]any{"type": "string"}
	}
	if d.Default != nil {
		out["default"] = d.Default
	}
	return out
}

// Args is one parsed call: the DSL plus the values that fill its references.
type Args struct {
	Cmd        string
	As         string
	Timeout    float64
	Limit      int
	TotalLimit int
	LearnMode  bool
	Structured bool
	Content    bool
	Values     []string
	Vars       map[string]string
	Secrets    map[string]string
}

// ParamError is a refusal about the ARGUMENTS (not about the cmd). It is
// reported as one 400 slot listing every problem, because a caller that sent
// three bad values should not need three round trips to learn that.
type ParamError struct {
	Problems []string
}

func (e *ParamError) Error() string { return strings.Join(e.Problems, "; ") }

// Parse reads the raw arguments and refuses every value outside its domain.
//
// It reads the raw JSON rather than decoding into a struct, because decoding
// absorbs two different mistakes: `timeout: "abc"` becomes the zero value and
// the call silently gets a different window than the one it asked for. Absent
// takes the table's default (a documented fact); present-and-wrong is a 400 that
// names the argument.
func Parse(raw json.RawMessage) (*Args, *ParamError) {
	a := &Args{
		Timeout:    0,
		Limit:      20,
		TotalLimit: 100,
		Content:    true,
		Vars:       map[string]string{},
		Secrets:    map[string]string{},
	}
	if len(raw) == 0 || string(raw) == "null" {
		return a, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, &ParamError{Problems: []string{
			fmt.Sprintf("arguments 必须是一个 JSON 对象：%v", err),
		}}
	}

	var problems []string
	for _, name := range sortedKeys(fields) {
		d, known := declFor(name)
		if !known {
			problems = append(problems, fmt.Sprintf("未知参数 %q（%s）", name, knownNames()))
			continue
		}
		if p := decodeInto(a, d, fields[name]); p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		return nil, &ParamError{Problems: problems}
	}

	if !a.Content && !a.Structured {
		return nil, &ParamError{Problems: []string{
			"content:false 与 structured_content:false 同时给出，等于拒绝了回复的每一个字节；" +
				"只想要 JSON 就传 structured_content:true，或者保持 content 为 true",
		}}
	}
	return a, nil
}

// decodeInto writes one argument into a, or returns a problem sentence.
func decodeInto(a *Args, d Decl, raw json.RawMessage) string {
	switch d.Kind {
	case KindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Sprintf("%s 必须是字符串，收到 %s", d.Name, truncate(raw))
		}
		if len(d.Enum) > 0 && !contains(d.Enum, s) {
			return fmt.Sprintf("%s 只能是 %s，收到 %q", d.Name, strings.Join(d.Enum, " / "), s)
		}
		if d.Name == "cmd" {
			a.Cmd = s
		}
		if d.Name == "as" {
			a.As = s
		}
	case KindNumber:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Sprintf("%s 必须是数字，收到 %s", d.Name, truncate(raw))
		}
		if d.Min != nil && n < *d.Min {
			return fmt.Sprintf("%s 不能小于 %v，收到 %v", d.Name, *d.Min, n)
		}
		switch d.Name {
		case "timeout":
			a.Timeout = n
		case "limit":
			a.Limit = int(n)
		case "total_limit":
			a.TotalLimit = int(n)
		}
	case KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Sprintf("%s 必须是布尔值，收到 %s", d.Name, truncate(raw))
		}
		switch d.Name {
		case "learn_mode":
			a.LearnMode = b
		case "structured_content":
			a.Structured = b
		case "content":
			a.Content = b
		}
	case KindStringArray:
		var vs []string
		if err := json.Unmarshal(raw, &vs); err != nil {
			return fmt.Sprintf("%s 必须是字符串数组，收到 %s", d.Name, truncate(raw))
		}
		if d.Name == "args" {
			a.Values = vs
		}
	case KindStringMap:
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Sprintf("%s 必须是 string→string 的对象，收到 %s", d.Name, truncate(raw))
		}
		switch d.Name {
		case "vars":
			a.Vars = m
		case "secrets":
			a.Secrets = m
		}
	}
	return ""
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func knownNames() string {
	names := make([]string, 0, len(Decls))
	for _, d := range Decls {
		names = append(names, d.Name)
	}
	return "已知：" + strings.Join(names, ", ")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func truncate(b []byte) string {
	const max = 60
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
