package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"scode/internal/llm"
)

// Real-API integration test. Skipped unless SCODE_INTEGRATION=1 and
// ANTHROPIC_API_KEY (or SCODE_ANTHROPIC_KEY) are set — the pi pattern of
// env-gated live tests. SCODE_ANTHROPIC_BASE_URL redirects to a
// compatible endpoint (e.g. GLM's Anthropic API); SCODE_ANTHROPIC_MODEL
// picks the model (default: claude-sonnet-4-5).
func integrationEnv(t *testing.T) (key, base, model string) {
	t.Helper()
	if os.Getenv("SCODE_INTEGRATION") != "1" {
		t.Skip("SCODE_INTEGRATION != 1; skipping live test")
	}
	key = os.Getenv("SCODE_ANTHROPIC_KEY")
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	if key == "" {
		t.Skip("no Anthropic key available")
	}
	base = os.Getenv("SCODE_ANTHROPIC_BASE_URL")
	model = os.Getenv("SCODE_ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-sonnet-4-5"
	}
	return key, base, model
}

// The M1 acceptance criterion: on the second turn of one session the
// provider reports cache_read_input_tokens > 0 — the prefix-stable
// transcript plus breakpoint placement is doing its job.
func TestIntegrationCacheHitOnSecondTurn(t *testing.T) {
	key, base, modelID := integrationEnv(t)

	p := New(key, base)
	model := llm.Model{ID: modelID, Provider: "anthropic", APIShape: "anthropic-messages"}

	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "You are scode, a terse coding assistant. Answer in one short sentence.",
		Tools: []llm.Tool{
			{Name: "read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("Remember the codeword: pinecone-42. Just acknowledge.")}, TS: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	turn := func() *llm.Message {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		events, err := p.Stream(ctx, model, tr, llm.StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var final *llm.Message
		for ev := range events {
			if ev.Terminal() {
				final = ev.Message
			}
		}
		if final == nil || final.StopReason != llm.StopEndTurn {
			t.Fatalf("turn failed: %+v", final)
		}
		if err := tr.Append(*final); err != nil {
			t.Fatal(err)
		}
		return final
	}

	turn()
	if err := tr.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("What was the codeword? One word.")}, TS: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	second := turn()

	if second.Usage == nil || second.Usage.CacheRead == 0 {
		t.Fatalf("second turn shows no cache read: %+v", second.Usage)
	}
	if !strings.Contains(strings.ToLower(messagePlainText(*second)), "pinecone") {
		t.Fatalf("model lost context across turns: %q", messagePlainText(*second))
	}
	t.Logf("usage turn2: %+v", second.Usage)
}

func messagePlainText(m llm.Message) string {
	var sb strings.Builder
	for _, b := range m.Content {
		if b.Kind == llm.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
