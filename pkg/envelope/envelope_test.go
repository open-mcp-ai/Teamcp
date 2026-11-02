package envelope

import (
	"strings"
	"testing"
)

func slot() Slot {
	return Slot{
		Code:    200,
		Headers: map[string]string{"Path": "send acme/dev", "Seq": "17", "Next-Seq": "18"},
		Body:    "ok",
	}
}

func TestStatusLineIsTeam(t *testing.T) {
	if got, want := slot().StatusLine(), "TEAM/1 200 OK"; got != want {
		t.Fatalf("StatusLine() = %q, want %q", got, want)
	}
}

// 429 是本项目新增的码：速率限制不是 Termcp 的词汇，但 Teamcp 每 agent 限速要用它。
func TestReasonTableCoversTeamcpCodes(t *testing.T) {
	for code, want := range map[int]string{
		202: "Accepted",
		204: "No Content",
		206: "Partial Content",
		208: "Already Reported",
		429: "Too Many Requests",
	} {
		if got := (Slot{Code: code}).StatusLine(); !strings.HasSuffix(got, want) {
			t.Errorf("code %d rendered %q, want suffix %q", code, got, want)
		}
	}
}

// 两个投影必须携带同一组键、同一个 body —— 否则只读结构化投影的客户端会缺数据。
func TestTwoProjectionsAgree(t *testing.T) {
	s := slot()
	s.Headers["Unknown-Header"] = "x"
	text := s.Text()
	structured := s.Structured()

	for _, key := range []string{"Path", "Seq", "Next-Seq", "Unknown-Header"} {
		if !strings.Contains(text, key+": ") {
			t.Errorf("text projection is missing header %q", key)
		}
		if _, ok := structured[key]; !ok {
			t.Errorf("structured projection is missing header %q", key)
		}
	}
	if structured["body"] != s.Body {
		t.Errorf("structured body = %v, want %q", structured["body"], s.Body)
	}
	if structured["protocol"] != Protocol || structured["code"] != s.Code {
		t.Errorf("structured status = %v/%v", structured["protocol"], structured["code"])
	}
}

// Seq / Next-Seq 在结构化投影里必须是数字，其余保持字符串。
func TestNumericHeadersStayNumbers(t *testing.T) {
	structured := slot().Structured()
	if structured["Seq"] != int64(17) {
		t.Errorf("Seq = %v (%T), want int64(17)", structured["Seq"], structured["Seq"])
	}
	if structured["Path"] != "send acme/dev" {
		t.Errorf("Path = %v, want string", structured["Path"])
	}
}

// 渲染顺序必须确定：两次同样的 slot 逐字节相同，且已知标头排在未知标头之前。
func TestRenderOrderIsDeterministic(t *testing.T) {
	mk := func() Slot {
		return Slot{Code: 200, Headers: map[string]string{
			"Zz-Unknown": "1", "Path": "whoami", "Aa-Unknown": "2", "Room": "acme/dev",
		}}
	}
	first, second := mk().Text(), mk().Text()
	if first != second {
		t.Fatalf("two identical slots rendered differently:\n%q\n%q", first, second)
	}
	pathAt := strings.Index(first, "Path: ")
	zzAt := strings.Index(first, "Zz-Unknown: ")
	aaAt := strings.Index(first, "Aa-Unknown: ")
	if !(pathAt < zzAt && aaAt < zzAt) {
		t.Errorf("known header should precede unknown ones, and unknowns sort: path=%d aa=%d zz=%d", pathAt, aaAt, zzAt)
	}
}

// 这是从 Termcp 适配时改掉的最重要的一处：消息方言下按字节截断正文，
// 绝不能改写按「条」计数的 Content-Range —— 否则等于告诉调用方从一个它没拿到的窗口继续。
func TestCutBodyLeavesMessageRangeAlone(t *testing.T) {
	s := Slot{
		Code:    206,
		Headers: map[string]string{"Content-Range": ContentRange(1, 20, 57)},
		Body:    strings.Repeat("a", 100),
	}
	cut := s.CutBody(40)

	if len(cut.Body) != 40 {
		t.Fatalf("body length = %d, want 40", len(cut.Body))
	}
	if got, want := cut.Headers["Truncated"], "60 bytes withheld"; got != want {
		t.Errorf("Truncated = %q, want %q", got, want)
	}
	if cut.Headers["Content-Range"] != "messages 1-20/57" {
		t.Errorf("Content-Range was rewritten to %q; a byte cut must not move a message window",
			cut.Headers["Content-Range"])
	}
}

// 不设上限时不改动任何东西。
func TestCutBodyNoOpWhenUnderLimit(t *testing.T) {
	s := slot()
	cut := s.CutBody(-1)
	if cut.Body != s.Body {
		t.Errorf("negative keep changed the body")
	}
	if _, ok := cut.Headers["Truncated"]; ok {
		t.Errorf("negative keep set Truncated")
	}
}

// end+1 就是下一个 --since。
func TestRangeEndGivesNextCursor(t *testing.T) {
	got, ok := RangeEnd(ContentRange(1, 20, 57))
	if !ok || got != 21 {
		t.Fatalf("RangeEnd(messages 1-20/57) = %v,%v; want 21,true", got, ok)
	}
	if _, ok := RangeEnd("bytes 1-20/57"); ok {
		t.Errorf("a bytes range must not parse as a message range")
	}
	if got := UnsatisfiedRange(57); got != "messages */57" {
		t.Errorf("UnsatisfiedRange(57) = %q", got)
	}
}

// 方言是可替换的（这也是这套渲染器能同时服务两套协议的原因）。
func TestDialectOverride(t *testing.T) {
	term := Dialect{Protocol: "TERM/1", Unit: "bytes"}
	s := Slot{Code: 206, Headers: map[string]string{"Content-Range": RangeIn(term, 0, 99, 640)}}

	if got := s.StatusLineIn(term); got != "TERM/1 206 Partial Content" {
		t.Errorf("StatusLineIn = %q", got)
	}
	if got, ok := RangeEndIn(term, "bytes 0-99/640"); !ok || got != 100 {
		t.Errorf("RangeEndIn = %v,%v; want 100,true", got, ok)
	}
}

// 一条回复 = 多个 slot，slot 之间一个空行；任一个失败则整体 isError。
func TestReply(t *testing.T) {
	r := Reply{Slots: []Slot{{Code: 200, Body: "a"}, {Code: 204, Body: ""}}}
	text := r.Text()
	if !strings.Contains(text, "\n\nTEAM/1 204") {
		t.Errorf("slots are not separated by exactly one blank line: %q", text)
	}
	if r.IsError() {
		t.Errorf("2xx reply reported as error")
	}

	r.Slots = append(r.Slots, Slot{Code: 403})
	if !r.IsError() {
		t.Errorf("reply containing a 403 must report an error")
	}
}

// 206 是成功而非失败：它交付了请求的那个窗口。
func TestPartialContentIsNotAnError(t *testing.T) {
	if (Slot{Code: 206}).IsError() {
		t.Errorf("206 reported as an error")
	}
}
