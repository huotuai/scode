package openaic

import (
	"testing"

	"scode/internal/llm"
)

func TestDialectUsageVariants(t *testing.T) {
	cases := []struct {
		name string
		json string
		want llm.Usage
	}{
		{
			"standard cached_tokens",
			`{"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":400}}}`,
			llm.Usage{Input: 100, Output: 20, CacheRead: 400},
		},
		{
			"deepseek prompt_cache_hit_tokens",
			`{"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":20,"prompt_cache_hit_tokens":300}}`,
			llm.Usage{Input: 200, Output: 20, CacheRead: 300},
		},
		{
			"kimi top-level cached_tokens",
			`{"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":20,"cached_tokens":250}}`,
			llm.Usage{Input: 250, Output: 20, CacheRead: 250},
		},
		{
			"openrouter cache_write_tokens",
			`{"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":100,"cache_write_tokens":50}}}`,
			llm.Usage{Input: 350, Output: 20, CacheRead: 100, CacheWrite: 50},
		},
	}
	for _, c := range cases {
		a := newAssembler("m")
		_, _, err := a.handle(c.json)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		a.handle("[DONE]") //nolint:errcheck
		got := a.snapshot().Usage
		if got == nil {
			t.Fatalf("%s: no usage", c.name)
		}
		if got.Input != c.want.Input || got.Output != c.want.Output || got.CacheRead != c.want.CacheRead || got.CacheWrite != c.want.CacheWrite {
			t.Errorf("%s: got %+v, want %+v", c.name, *got, c.want)
		}
	}
}

func TestDialectReasoningFields(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
		a := newAssembler("m")
		if _, _, err := a.handle(`{"choices":[{"index":0,"delta":{"role":"assistant","` + field + `":"think"}}]}`); err != nil {
			t.Fatal(err)
		}
		a.handle("[DONE]") //nolint:errcheck
		msg := a.snapshot()
		if len(msg.Content) == 0 || msg.Content[len(msg.Content)-1].Kind != llm.BlockThinking || msg.Content[len(msg.Content)-1].Text != "think" {
			t.Fatalf("field %s: thinking lost: %+v", field, msg.Content)
		}
		// pi's thinkingSignature: the arrival field rides the block's
		// Signature so the request builder replays under the same key.
		if got := msg.Content[len(msg.Content)-1].Signature; got != field {
			t.Fatalf("field %s: signature = %q", field, got)
		}
	}
}
