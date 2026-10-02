package tools

import (
	"fmt"
	"strings"
	"testing"

	"scode/internal/agent"
	"scode/internal/llm"
)

func TestShrinkOversizedResultSkipsSmallAndError(t *testing.T) {
	small := agent.TextResult("tiny")
	if got := ShrinkOversizedResult(llm.Block{}, small); got.Content[0].Text != "tiny" {
		t.Fatal("small result must pass through")
	}
	big := strings.Repeat("line\n", 20000)
	errRes := agent.ErrorResult(big)
	if got := ShrinkOversizedResult(llm.Block{}, errRes); !got.IsError || got.Content[0].Text != big {
		t.Fatal("error results must pass through untouched")
	}
}

func TestShrinkOversizedResultElidesMiddle(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&sb, "match %d: some reasonably long line of grep output text\n", i)
	}
	full := sb.String() // ~160KB — above the threshold
	res := agent.TextResult(full)

	got := ShrinkOversizedResult(llm.Block{}, res)
	text := got.Content[0].Text
	if len(text) >= len(full) {
		t.Fatalf("no shrinkage: %d >= %d", len(text), len(full))
	}
	if !strings.Contains(text, ShrinkResultMarker) {
		t.Fatal("elision marker missing")
	}
	if !strings.HasPrefix(text, "match 1:") {
		t.Fatal("head must be preserved from line 1")
	}
	if !strings.Contains(text, "match 3000:") {
		t.Fatal("tail must keep the final lines")
	}
	if strings.Contains(text, "match 1500:") {
		t.Fatal("middle lines must be elided")
	}
	// Idempotent: shrinking the shrunk result changes nothing.
	again := ShrinkOversizedResult(llm.Block{}, got)
	if again.Content[0].Text != text {
		t.Fatal("shrink must be idempotent")
	}
	// The original result is immutable.
	if res.Content[0].Text != full {
		t.Fatal("input result was mutated")
	}
}

func TestMiddleElideHugeSingleLine(t *testing.T) {
	big := strings.Repeat("x", 100*1024)
	got := middleElide(big)
	if len(got) >= len(big) || !strings.Contains(got, ShrinkResultMarker) {
		t.Fatalf("single huge line not elided: %d bytes", len(got))
	}
}

func TestMiddleElideNoopNearThreshold(t *testing.T) {
	// Few lines whose total sits under the byte-elision floor: the
	// byte-level branch keeps them as-is.
	lines := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		lines = append(lines, strings.Repeat("y", 680))
	}
	s := strings.Join(lines, "\n") // ~20KB, 30 lines
	if got := middleElide(s); got != s {
		t.Fatal("few-line input under the byte floor must pass through")
	}
}
