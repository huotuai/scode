package agent

import (
	"context"
	"strings"
	"time"

	"scode/internal/llm"
)

// titleSystemPrompt steers the one-off titling call. The model is asked for
// a bare, short title in the user's language so it can be shown verbatim in
// the desktop history list.
const titleSystemPrompt = "You name coding-assistant conversations. Reply with ONLY a concise title for the user's request: at most 16 characters, no quotes, no trailing punctuation, in the same language as the request. No explanations."

// SideChannel runs a one-off, no-tools text call (title generation and
// memory extraction use it): caching disabled, thinking off, the raw
// text returned.
func (a *Agent) SideChannel(ctx context.Context, systemPrompt, userText string) (string, error) {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return "", nil
	}
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: systemPrompt,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock(userText)}, TS: time.Now().UnixMilli()},
		},
	})
	if err != nil {
		return "", err
	}
	return a.streamSummary(ctx, tr, true)
}

// GenerateTitle asks the model for a short title derived from the first user
// message. It is a best-effort one-off call (no tools, caching disabled)
// mirroring the compaction path's economics. An empty title is not an error.
func (a *Agent) GenerateTitle(ctx context.Context, userText string) (string, error) {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return "", nil
	}
	if r := []rune(userText); len(r) > 4000 {
		userText = string(r[:4000])
	}
	out, err := a.SideChannel(ctx, titleSystemPrompt, userText)
	if err != nil {
		return "", err
	}
	return SanitizeTitle(out), nil
}

// SanitizeTitle normalizes a model-produced title: first line only,
// whitespace collapsed, surrounding quote/markdown characters and a leading
// "标题:"/"title:" label stripped, length capped.
func SanitizeTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	for _, p := range []string{"标题:", "标题：", "title:", "Title:", "Title："} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(s[len(p):])
			break
		}
	}
	s = strings.Trim(s, " \t`\"'“”‘’「」『』")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 24 {
		s = strings.TrimSpace(string(r[:24]))
	}
	return s
}
