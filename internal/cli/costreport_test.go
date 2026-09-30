package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scode/internal/llm"
)

// /cost shows every category the usage kernel tracks: token accounting
// per cache class, cost, context occupancy against the window, the
// cache hit rate, and the six-row context breakdown.
func TestCostReportAllCategories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	app.spent = llm.Usage{
		Input: 12345, Output: 678, CacheRead: 234567, CacheWrite: 4567, CacheWrite1h: 890,
	}
	rep := app.CostReport()
	for _, want := range []string{
		"输入 12,345", "输出 678", "缓存读 234,567", "缓存写 4,567", "缓存写(1h) 890",
		"上下文:", "缓存命中 95%", // 234,567 / (12,345 + 234,567), rounded
		"明细:", "消息", "系统提示词", "系统工具", "技能", "MCP 工具", "其他",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("report missing %q:\n%s", want, rep)
		}
	}
	// No pricing configured in the test settings → no cost segment.
	if strings.Contains(rep, "$") {
		t.Fatalf("cost shown without pricing:\n%s", rep)
	}
	// Three lines: usage, context, breakdown.
	if n := strings.Count(rep, "\n") + 1; n != 3 {
		t.Fatalf("report = %d lines, want 3:\n%s", n, rep)
	}

	// The 1h cache-write class drops out when the provider never
	// reports it; everything else stays.
	app.spent.CacheWrite1h = 0
	if rep = app.CostReport(); strings.Contains(rep, "缓存写(1h)") {
		t.Fatalf("zero 1h cache write should be omitted:\n%s", rep)
	}

	// The context line adapts to a known vs unknown window.
	rep = app.CostReport()
	if app.Model.ContextWindow > 0 {
		if !strings.Contains(rep, "%)") {
			t.Fatalf("known window should render a percentage:\n%s", rep)
		}
	} else if !strings.Contains(rep, "窗口未知") {
		t.Fatalf("unknown window should degrade to the bare count:\n%s", rep)
	}
}

func TestCommaInt(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1,000"}, {1234567, "1,234,567"},
		{-9876, "-9,876"},
	}
	for _, c := range cases {
		if got := commaInt(c.n); got != c.want {
			t.Errorf("commaInt(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
