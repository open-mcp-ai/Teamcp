// Package dsl parses the one `cmd` line into statements.
//
// THE RULE THAT MAKES THIS SAFE IS THE ORDER OF OPERATIONS: split → tokenize →
// substitute. A value is substituted into an already-tokenized word, so nothing a
// peer wrote can ever be re-read as syntax. That is why this parser needs no
// escaping machinery and no sentinel tokens — the value never reaches the grammar.
//
// A reference must therefore OWN its token. `$1` is a value; `prefix$1` is an
// error, not an interpolation, because half-a-value inside a word is exactly the
// case where "is this syntax or data?" stops having an answer.
package dsl

import (
	"fmt"
	"sort"
	"strings"
)

// Conn is how a statement is joined to the previous one.
type Conn string

const (
	Seq     Conn = ";"  // 总是执行
	AndThen Conn = "&&" // 上一条成功才执行
	OrElse  Conn = "||" // 上一条失败才执行
)

// Statement is one parsed command.
type Statement struct {
	// Verb is the first word: the directive name.
	Verb string
	// Args are the positional words after the verb, in order.
	Args []string
	// Flags are `--name=value` / `--name value` / bare `--name` (value "true").
	Flags map[string]string
	// Conn joins this statement to the previous one. The first statement is Seq.
	Conn Conn
	// Text is the statement AS WRITTEN, references not substituted.
	//
	// It must not carry the substituted values, for two reasons that both matter:
	// a body containing `;` would come back looking like syntax, and a value that
	// came in through `secrets` would be echoed into the reply — which is exactly
	// what putting it in `secrets` was meant to prevent.
	Text string
}

// Flag returns a flag's value, or "" when absent.
func (s Statement) Flag(name string) string { return s.Flags[name] }

// Values are the arguments' values that fill `$1`, `$NAME` and `$SECRETS`.
type Values struct {
	Positional []string
	Named      map[string]string
	Secrets    map[string]string
}

// Error is a parse refusal. It carries the offending fragment so a caller can
// find it without counting characters.
type Error struct {
	Msg  string
	Near string
}

func (e *Error) Error() string {
	if e.Near == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s（在 %q 附近）", e.Msg, e.Near)
}

// Parse turns one cmd line into statements.
//
// Refusals, all of which the caller should fix rather than retry:
//   - an unknown `$N` / `$NAME`
//   - a provided value that nothing references (a dropped argument is invisible
//     in the reply, so it must be refused up front)
//   - a token that contains a reference but is not one
//   - a `--flag` whose value is missing at the end of a statement
func Parse(cmd string, v Values) ([]Statement, error) {
	segments, err := splitSegments(cmd)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, nil
	}

	usedPos, usedNamed := map[int]bool{}, map[string]bool{}
	out := make([]Statement, 0, len(segments))

	for _, seg := range segments {
		words, err := tokenize(seg.text)
		if err != nil {
			return nil, err
		}
		if len(words) == 0 {
			continue // `a ; ; b` — an empty statement is not an instruction
		}

		resolved := make([]string, 0, len(words))
		for _, w := range words {
			val, err := substitute(w, v, usedPos, usedNamed)
			if err != nil {
				return nil, err
			}
			resolved = append(resolved, val)
		}

		st := Statement{Verb: resolved[0], Flags: map[string]string{}, Conn: seg.conn}
		rest := resolved[1:]

		for i := 0; i < len(rest); i++ {
			w := rest[i]
			if !strings.HasPrefix(w, "--") || w == "--" {
				st.Args = append(st.Args, w)
				continue
			}
			name, val, hasVal := strings.Cut(strings.TrimPrefix(w, "--"), "=")
			if name == "" {
				return nil, &Error{Msg: "空的 flag 名：`--=x` 不是参数", Near: w}
			}
			if !hasVal {
				// `--flag value` — but only when the next word is not itself a flag,
				// so `send --kind ask --to coder` reads as two flags plus one value.
				if i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") {
					val = rest[i+1]
					i++
				} else {
					val = "true"
				}
			}
			if _, dup := st.Flags[name]; dup {
				// Last-one-wins would be silent. Two spellings of one flag is a
				// caller mistake worth naming.
				return nil, &Error{Msg: "flag `--" + name + "` 在同一条语句里出现了两次", Near: w}
			}
			st.Flags[name] = val
		}

		st.Text = seg.text
		out = append(out, st)
	}

	if err := checkUnused(v, usedPos, usedNamed); err != nil {
		return nil, err
	}
	return out, nil
}

// substitute replaces one word that IS a reference. It refuses words that merely
// contain one, which is the rule the package comment explains.
func substitute(w string, v Values, usedPos map[int]bool, usedNamed map[string]bool) (string, error) {
	if !strings.Contains(w, "$") {
		return w, nil
	}

	if w == "$" {
		return "", &Error{Msg: "`$` 后面没有名字：要一个位置号（$1）或名字（$NAME）"}
	}
	if !strings.HasPrefix(w, "$") {
		return "", &Error{Msg: "引用必须独占一个词：`$1` 可以，`前缀$1` 不行。" +
			"因为值是不可信内容，混在词里就分不清哪部分是语法", Near: w}
	}

	ref := w[1:]
	braced := strings.HasPrefix(ref, "{") && strings.HasSuffix(ref, "}") && len(ref) > 2
	if braced {
		ref = ref[1 : len(ref)-1]
	}

	if allDigits(ref) {
		n, ok := atoi(ref)
		if !ok || n == 0 {
			return "", &Error{Msg: "位置值从 $1 开始，$0 没有意义", Near: w}
		}
		if n > len(v.Positional) {
			return "", &Error{Msg: fmt.Sprintf("$%d 没有对应的值（只给了 %d 个）", n, len(v.Positional)), Near: w}
		}
		usedPos[n-1] = true
		return v.Positional[n-1], nil
	}

	if !isIdent(ref) {
		return "", &Error{Msg: fmt.Sprintf("%q 不是合法的名字（要 [A-Za-z_][A-Za-z0-9_]*）", ref), Near: w}
	}
	if s, ok := v.Secrets[ref]; ok {
		usedNamed[ref] = true
		return s, nil
	}
	if s, ok := v.Named[ref]; ok {
		usedNamed[ref] = true
		return s, nil
	}
	return "", &Error{Msg: fmt.Sprintf("$%s 没有定义：变量的值放在 args、vars 或 secrets 里", ref), Near: w}
}

// checkUnused refuses values nothing referenced.
//
// A value that reaches the server and never appears in the reply is invisible: the
// caller believes it sent something and cannot tell that it did not. Refusing is
// the only reporting mechanism available, so it is used.
func checkUnused(v Values, usedPos map[int]bool, usedNamed map[string]bool) error {
	var problems []string

	for i := range v.Positional {
		if !usedPos[i] {
			problems = append(problems, fmt.Sprintf("$%d", i+1))
		}
	}

	var named []string
	for name := range v.Named {
		if !usedNamed[name] {
			named = append(named, "$"+name)
		}
	}
	sort.Strings(named)
	problems = append(problems, named...)

	for name := range v.Secrets {
		if !usedNamed[name] {
			problems = append(problems, "$"+name)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return &Error{Msg: "这些值给了但 cmd 没有引用：" + strings.Join(problems, ", ") +
		"（一个被丢掉的值在回复里看不见，所以直接拒绝）"}
}

// segment is one statement's raw text plus the connector before it.
type segment struct {
	text string
	conn Conn
}

// splitSegments cuts on `;`, `&&` and `||` outside quotes.
//
// It does NOT try to be a shell: no subshells, no pipes, no redirection. Those
// are additive later; leaving them out now keeps "one statement = one slot" true.
func splitSegments(cmd string) ([]segment, error) {
	var out []segment
	var cur strings.Builder
	conn := Seq
	var quote rune

	flush := func() {
		t := strings.TrimSpace(cur.String())
		cur.Reset()
		if t != "" {
			out = append(out, segment{text: t, conn: conn})
			conn = Seq
		}
	}

	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			cur.WriteRune(c)
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			cur.WriteRune(c)
		case c == ';':
			flush()
		case c == '&' && i+1 < len(rs) && rs[i+1] == '&':
			flush()
			conn = AndThen
			i++
		case c == '|' && i+1 < len(rs) && rs[i+1] == '|':
			flush()
			conn = OrElse
			i++
		case c == '|':
			return nil, &Error{Msg: "管道 `|` 还没有实现；用 `;` 或 `&&` 组合", Near: string(c)}
		default:
			cur.WriteRune(c)
		}
	}
	if quote != 0 {
		return nil, &Error{Msg: "引号没有闭合"}
	}
	flush()
	return out, nil
}

// tokenize splits one statement into words, dropping quoting.
//
// Quoting only groups words: it is not preserved into the value, because a value
// arrives through args/vars, never through the cmd text (ADR-011).
func tokenize(seg string) ([]string, error) {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false

	rs := []rune(seg)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			if c == '\\' && i+1 < len(rs) && quote == '"' {
				i++
				cur.WriteRune(rs[i])
				continue
			}
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, &Error{Msg: "引号没有闭合"}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) (int, bool) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 0, false
		}
	}
	return n, true
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_':
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
