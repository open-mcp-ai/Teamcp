// Package envelope renders one tool reply slot as an HTTP-style response.
//
// The format is HTTP because a model has decades of prior on it: a status line
// ("TEAM/1 206 Partial Content"), header lines, a blank line, then the body. A
// status code says what happened to THIS statement; the headers only carry what
// is news (a cursor it did not know, a room it was not told) or what changes the
// next decision (how many messages remain, so it knows to continue with --since).
//
// Two projections of one value: Text for the model, Structured for a client that
// wants to branch programmatically. Both are produced HERE, from one value, so
// they cannot disagree about which keys exist, and both carry the body verbatim.
// Omitting it from Structured would lose the bytes for a client that renders
// only that projection.
//
// PROVENANCE: adapted from Termcp's internal/envelope (MIT, github.com/open-mcp-ai/termcp),
// which speaks TERM/1 over byte streams. Two things changed in the adaptation, and
// both are consequences of Teamcp's unit being a MESSAGE rather than a byte:
//
//  1. Content-Range counts messages ("messages 1-20/57"), and `end+1` is the next
//     --since. The cursor arithmetic is the same question as Termcp's `end+1`,
//     which is why the range form was worth keeping at all.
//  2. CutBody replaces Cut as the truncation path. Cutting a message window's
//     TEXT by N bytes must not rewrite a header that counts MESSAGES — that
//     would report a window it did not deliver. So the two operations are
//     separate here: CutBody withholds bytes and says so in `Truncated`, and
//     the range header stays true.
package envelope

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Protocol is the version token on the status line.
const Protocol = "TEAM/1"

// Dialect is what one protocol renders differently. Everything else — the status
// line shape, the header order, the header/body separator, the two projections —
// is shared, because that shared shape is the whole reason this package exists.
//
// THE ZERO VALUE IS TEAM/1 + messages. A consumer that says nothing gets this
// project's behaviour; a consumer that speaks another dialect names it.
type Dialect struct {
	// Protocol is the leading tag on the status line. Empty means Protocol.
	Protocol string
	// Unit is the unit noun in Content-Range. Empty means "messages".
	Unit string
	// Reasons overrides or extends the canonical phrase table. Nil means the
	// built-in table only. It is an override rather than a replacement so a
	// consumer can rename one code without restating the others.
	Reasons map[int]string
}

// TeamDialect is TEAM/1 spelled explicitly, for a caller that prefers to name
// what the zero value already means.
var TeamDialect = Dialect{Protocol: Protocol, Unit: "messages"}

func (d Dialect) protocol() string {
	if d.Protocol == "" {
		return Protocol
	}
	return d.Protocol
}

func (d Dialect) unit() string {
	if d.Unit == "" {
		return "messages"
	}
	return d.Unit
}

// Slot is one response unit: one statement in a cmd, one room in a batch.
type Slot struct {
	// Code is the HTTP status code (200, 204, 403, …).
	Code int
	// Reason is the status line's text; empty means the canonical phrase.
	Reason string
	// Headers are the envelope fields, keyed by their wire name. Order in the
	// rendered text is fixed by headerOrder so two identical replies are
	// byte-identical.
	Headers map[string]string
	// Body is the raw payload: rendered messages, or generated text (a `whoami`
	// card, a `help <verb>` page).
	Body string
}

// canonicalReason is the phrase for every code the tool can return. Keeping the
// table here (rather than relying on net/http's) means the wire format does not
// change if the standard library renames a phrase.
var canonicalReason = map[int]string{
	200: "OK",
	202: "Accepted",
	204: "No Content",
	206: "Partial Content",
	208: "Already Reported",
	400: "Bad Request",
	403: "Forbidden",
	404: "Not Found",
	409: "Conflict",
	413: "Payload Too Large",
	429: "Too Many Requests",
	500: "Internal Server Error",
	501: "Not Implemented",
}

// headerOrder is the render order, and the reason a reply is reproducible: the
// status line, then the address, then the cursor, then the window, then the
// teaching/diagnosis lines. docs/protocol.md §3 documents these in this order.
//
// A header this build does not know about renders after the known ones in
// alphabetical order, so a new header is never silently dropped from the text
// while appearing in the structured projection.
var headerOrder = []string{
	// 寻址
	"Path", "Agent", "Room",
	// 游标
	"Seq", "Next-Seq", "Content-Range",
	// 等待
	"Waited", "Timed-Out",
	// 边界
	"Truncated", "Omitted", "Retry-After",
	// 解释与下一步
	"Pending-ID", "Error-Code", "Hint", "Usage", "Next",
	// 终端语义的遗留名（保留以便两套信封同形）
	"Status", "Exit-Code", "Handle", "Still-Running", "Folded", "Sampled",
	"Executed", "Dry-Run", "Idempotent",
}

// numericHeaders are the header names whose structured projection is a number
// rather than a string. Nothing else is reinterpreted: a second interpretation
// of a value is a second truth.
var numericHeaders = map[string]bool{
	"Seq":       true,
	"Next-Seq":  true,
	"Exit-Code": true,
}

// reasonFor returns the phrase for a code.
func reasonFor(code int) string {
	if r, ok := canonicalReason[code]; ok {
		return r
	}
	return "Status"
}

// reasonFor returns the phrase for a code under this dialect: an override wins,
// otherwise the canonical table, otherwise the neutral "Status".
func (d Dialect) reasonFor(code int) string {
	if d.Reasons != nil {
		if r, ok := d.Reasons[code]; ok {
			return r
		}
	}
	return reasonFor(code)
}

// StatusLine is the first line of the rendered form.
func (s Slot) StatusLine() string { return s.StatusLineIn(TeamDialect) }

// StatusLineIn is StatusLine under d.
func (s Slot) StatusLineIn(d Dialect) string {
	reason := s.Reason
	if reason == "" {
		reason = d.reasonFor(s.Code)
	}
	return fmt.Sprintf("%s %d %s", d.protocol(), s.Code, reason)
}

// IsError reports whether this slot is a failure. A 206 that returned the
// requested window is a success even though it is partial, and a 500 whose
// statement failed is a failure even though the tool call itself worked.
func (s Slot) IsError() bool { return s.Code >= 400 }

// ContentRange renders the RFC 9110 form in d's unit.
//
// total < 0 means the length is unknown and is rendered as `*`. end is
// INCLUSIVE, as the RFC specifies, which is what makes `end+1` the next
// --since (or, in bytes, the next --cursor).
func ContentRange(start, end, total int64) string {
	return RangeIn(TeamDialect, start, end, total)
}

// RangeIn renders the range in d's unit.
func RangeIn(d Dialect, start, end, total int64) string {
	if total < 0 {
		return fmt.Sprintf("%s %d-%d/*", d.unit(), start, end)
	}
	return fmt.Sprintf("%s %d-%d/%d", d.unit(), start, end, total)
}

// UnsatisfiedRange renders the `*/total` form: a reply that carries no range (a
// 204), but where the caller still benefits from knowing how much there is.
// Without it, an empty reply says nothing about whether anything exists.
func UnsatisfiedRange(total int64) string {
	return UnsatisfiedRangeIn(TeamDialect, total)
}

// UnsatisfiedRangeIn is UnsatisfiedRange in d's unit.
func UnsatisfiedRangeIn(d Dialect, total int64) string {
	if total < 0 {
		return fmt.Sprintf("%s */*", d.unit())
	}
	return fmt.Sprintf("%s */%d", d.unit(), total)
}

// CutBody truncates the body to at most keep bytes and records how much was
// withheld in the `Truncated` header. It does NOT touch Content-Range.
//
// This is the message-dialect truncation path, and the separation from
// Content-Range is the point: the range counts messages, the body is text, and a
// byte cut cannot be expressed as a message count. Rewriting the range to
// describe the remaining bytes would tell the caller to resume from a window it
// was never given.
//
// keep < 0 means "no limit". The cut is byte-exact; callers that need it to land
// on a rune boundary must round first (a body cut mid-rune is still valid UTF-8
// at the transport layer, but a model reading a broken rune learns nothing).
func (s Slot) CutBody(keep int) Slot {
	if keep < 0 || keep >= len(s.Body) {
		return s
	}
	withheld := len(s.Body) - keep
	s.Body = s.Body[:keep]
	if s.Headers == nil {
		s.Headers = map[string]string{}
	}
	s.Headers["Truncated"] = fmt.Sprintf("%d bytes withheld", withheld)
	return s
}

// Cut truncates a BYTE-window body and rewrites Content-Range to describe what is
// actually returned. It is only meaningful in a byte dialect; in a message
// dialect use CutBody.
//
// The two must move together in a byte dialect: a range that claims bytes the
// body no longer carries is worse than no range at all, because the caller's next
// read starts where the header said and skips the rest of the stream.
func (s Slot) Cut(keep int) Slot { return s.CutIn(TeamDialect, keep) }

// CutIn is Cut under d.
func (s Slot) CutIn(d Dialect, keep int) Slot {
	if keep >= len(s.Body) {
		return s
	}
	if keep < 0 {
		keep = 0
	}
	start, _, ok := parseRangeIn(d, s.Headers["Content-Range"])
	if !ok {
		s.Body = s.Body[:keep]
		return s
	}
	total := rangeTotal(s.Headers["Content-Range"])
	s.Body = s.Body[:keep]
	s.Headers["Content-Range"] = RangeIn(d, start, start+int64(keep)-1, total)
	if keep == 0 {
		s.Headers["Content-Range"] = UnsatisfiedRangeIn(d, total)
	}
	return s
}

// parseRangeIn reads back a range this package wrote in d's unit. The unit prefix
// is checked against d rather than ignored: a header in another unit is not a
// range this dialect can continue from, and treating it as one would move a
// cursor by the wrong amount.
func parseRangeIn(d Dialect, v string) (start, end int64, ok bool) {
	v, found := strings.CutPrefix(v, d.unit()+" ")
	if !found {
		return 0, 0, false
	}
	span, _, found := strings.Cut(v, "/")
	if !found {
		return 0, 0, false
	}
	lo, hi, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(lo, 10, 64)
	end, err2 := strconv.ParseInt(hi, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return start, end, true
}

// RangeEnd returns the END of a range this package wrote, and reports whether the
// header held a range at all.
//
// The range is inclusive ("messages 1-20/57" ends at 20) while a cursor points at
// the next thing to deliver — so the cursor a caller advances to is end+1. That
// +1 is the whole reason this helper exists instead of each caller re-deriving it
// and one of them getting it off by one.
func RangeEnd(v string) (int64, bool) { return RangeEndIn(TeamDialect, v) }

// RangeEndIn is RangeEnd in d's unit.
func RangeEndIn(d Dialect, v string) (int64, bool) {
	_, end, ok := parseRangeIn(d, v)
	if !ok {
		return 0, false
	}
	return end + 1, true
}

// rangeTotal reads the total out of a range, or -1 when unknown. It is
// unit-agnostic on purpose: only the `/<total>` tail is read, so it works for any
// dialect without being told which one.
func rangeTotal(v string) int64 {
	_, total, found := strings.Cut(v, "/")
	if !found || total == "*" {
		return -1
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// Text renders the whole slot: status line, ordered headers, blank line, body.
//
// The body is appended verbatim. A body that does not end in a newline is NOT
// given one: the envelope describes a payload, and adding a byte would change it.
func (s Slot) Text() string { return s.TextIn(TeamDialect) }

// TextIn is Text under d.
func (s Slot) TextIn(d Dialect) string {
	var b strings.Builder
	b.WriteString(s.StatusLineIn(d))
	b.WriteByte('\n')
	for _, key := range orderedKeys(s.Headers) {
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(s.Headers[key])
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(s.Body)
	return b.String()
}

// orderedKeys returns the headers in render order, with any unknown header after
// the known ones in alphabetical order.
func orderedKeys(headers map[string]string) []string {
	known := make(map[string]bool, len(headerOrder))
	out := make([]string, 0, len(headers))
	for _, k := range headerOrder {
		known[k] = true
		if _, ok := headers[k]; ok {
			out = append(out, k)
		}
	}
	var extra []string
	for k := range headers {
		if !known[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// structValue converts a header's string value to the JSON type its name implies,
// so the two projections agree on the VALUE and not just the key.
func structValue(key, value string) any {
	if numericHeaders[key] {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return n
		}
	}
	return value
}

// Structured renders the slot as the JSON value that mirrors it: the status line
// as three keys, then every header by its own name, then the body.
//
// The body is always included. A client that renders only this projection is the
// reason the field exists, so omitting it would leave that client with no payload
// at all. A caller can decline the rendered text projection and keep this one;
// this one stays unconditional.
func (s Slot) Structured() map[string]any { return s.StructuredIn(TeamDialect) }

// StructuredIn is Structured under d.
func (s Slot) StructuredIn(d Dialect) map[string]any {
	out := make(map[string]any, len(s.Headers)+4)
	out["protocol"] = d.protocol()
	out["code"] = s.Code
	reason := s.Reason
	if reason == "" {
		reason = d.reasonFor(s.Code)
	}
	out["reason"] = reason
	for _, key := range orderedKeys(s.Headers) {
		out[key] = structValue(key, s.Headers[key])
	}
	out["body"] = s.Body
	return out
}

// Reply assembles slots into the tool result's two projections.
type Reply struct {
	Slots []Slot

	// StructuredContent also sends the reply as its JSON projection. It is OFF by
	// default, and that default is the fix rather than a preference: the two
	// projections carry the SAME bytes, so sending both bills every reply twice
	// and shows a human the same answer in two dialects on every call. The text
	// form is what a caller usually reads, so the JSON twin has to be asked for.
	StructuredContent bool

	// NoText suppresses the rendered text projection for a structured-only
	// caller. It is spelled NEGATIVELY on purpose: a Go zero value has to mean
	// "behave as before", and a Reply built by an early error return would
	// otherwise ship no text at all.
	NoText bool
}

// Text renders every slot, separated by exactly one blank line between responses
// (an HTTP message ends at its body; the separator is what makes two responses
// two).
func (r Reply) Text() string { return r.TextIn(TeamDialect) }

// TextIn is Text under d.
//
// The separator is normalized rather than joined: a body is appended VERBATIM
// (adding a byte to it would change the payload), so a body may or may not end in
// a newline, and `strings.Join(parts, "\n")` therefore produces a blank line only
// in the lucky case. Here the newline a body lacks is added to the SEPARATOR
// instead — the body keeps its bytes, and two slots are always separated by
// exactly one blank line.
func (r Reply) TextIn(d Dialect) string {
	var b strings.Builder
	for i, s := range r.Slots {
		if i > 0 {
			if !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		}
		b.WriteString(s.TextIn(d))
	}
	return b.String()
}

// Structured renders every slot as an array, index for index with Text's slots.
func (r Reply) Structured() []map[string]any { return r.StructuredIn(TeamDialect) }

// StructuredIn is Structured under d.
func (r Reply) StructuredIn(d Dialect) []map[string]any {
	out := make([]map[string]any, 0, len(r.Slots))
	for _, s := range r.Slots {
		out = append(out, s.StructuredIn(d))
	}
	return out
}

// IsError reports whether ANY slot failed. The tool result's isError is the OR of
// the slots: a batch where one statement failed is a reply that contains a
// failure, and hiding it behind a false would make the flag untrustworthy. The
// per-slot truth is still in each slot's status line and its structured twin.
func (r Reply) IsError() bool {
	for _, s := range r.Slots {
		if s.IsError() {
			return true
		}
	}
	return false
}
