package ratelimit_test

import (
	"testing"

	"github.com/open-mcp-ai/teamcp/internal/ratelimit"
)

func TestNilLimiterAllowsEverything(t *testing.T) {
	var l *ratelimit.Limiter
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("nil limiter refused a call")
	}
	if l := ratelimit.New(0); l != nil {
		t.Fatal("rate 0 should mean disabled, not a zero-capacity bucket")
	}
}

// 突发可以一次用掉一分钟的配额：读一批再逐条回答是可用的，不该被限速打断。
func TestBurstTheRefusesThenRecovers(t *testing.T) {
	l := ratelimit.New(3)
	var allowed int
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("acme/planner"); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("allowed %d of 5, want 3", allowed)
	}

	ok, wait := l.Allow("acme/planner")
	if ok {
		t.Fatal("the 6th call was allowed")
	}
	if wait <= 0 {
		t.Errorf("wait = %v, want > 0", wait)
	}

	// 另一个参与者有自己的桶
	if ok, _ := l.Allow("bob/reviewer"); !ok {
		t.Error("a different actor was charged for the first one's burst")
	}
}
