package llm

import "context"

// Capabilities describes what a provider's wire protocol supports so cache
// and transcript strategy can degrade per provider instead of per vendor
// (pi's compat-flag approach). Adding a provider never touches the
// transcript layer; it advertises capabilities and the request builder
// adapts.
type Capabilities struct {
	// CacheBreakpoints: the protocol accepts explicit cache markers
	// (Anthropic cache_control) that scode places on the last tool
	// declaration, the system blocks, and the final user message.
	CacheBreakpoints bool
	// ToolAdditions: tools can be declared mid-conversation, letting the
	// initial tools array stay byte-identical when the tool set grows.
	ToolAdditions bool
	// LongCacheRetention: 1h cache TTL is available (vs the 5m default).
	LongCacheRetention bool
	// MidConversationSystem: system messages may appear between user and
	// assistant messages; otherwise they are collapsed into the leading
	// prompt at the request boundary.
	MidConversationSystem bool
	// StreamingToolArguments: tool arguments arrive as incremental JSON
	// fragments during streaming (Anthropic input_json_delta).
	StreamingToolArguments bool
	// ImageInput: the model accepts image inputs (pi's model.input
	// containing "image"). Tool-result images forward only when set.
	ImageInput bool
	// AutoCache: the provider caches prompt prefixes automatically
	// server-side (DeepSeek/OpenAI/Gemini implicit caching) — one-off
	// summary calls can reuse the conversation prefix for free. Explicit
	// breakpoint providers (Anthropic cache_control) leave this false and
	// keep one-off calls CacheNone (pi's compaction economics).
	AutoCache bool
	// MidConvoSystem: the endpoint is VERIFIED to accept system/developer
	// messages after the conversation has started (pi's
	// compat.supportsMidConvoSystemMessages — default off there too).
	// On, later system deltas ride in place instead of folding into the
	// leading prompt, so the request head (and the implicit prefix
	// cache) survives mid-session section deltas. Unverified endpoints
	// may reject or silently drop mid-conversation system messages,
	// which is why this is an opt-in.
	MidConvoSystem bool
}

// CacheRetention selects the provider cache TTL, or disables caching.
type CacheRetention string

const (
	CacheShort CacheRetention = "short" // default TTL (5m on Anthropic)
	CacheLong  CacheRetention = "long"  // 1h where supported
	CacheNone  CacheRetention = "none"  // one-off calls (compaction summaries)
)

// Model identifies a model behind a provider. Wire details (endpoint paths,
// auth headers) live in the provider; the transcript layer only needs IDs.
type Model struct {
	ID        string `json:"id"`                  // provider-local model id
	Provider  string `json:"provider"`            // provider name (registry key)
	APIShape  string `json:"apiShape"`            // wire protocol family, e.g. "anthropic-messages", "openai-completions"
	MaxTokens int    `json:"maxTokens,omitempty"` // per-request output cap
	// ContextWindow is the model's input window in tokens (0 = unknown);
	// compaction thresholds derive from it when known.
	ContextWindow int `json:"contextWindow,omitempty"`
	// Reasoning marks a thinking-capable model (pi's model.reasoning).
	// Providers that carry a reasoning parameter gate it on this flag so
	// non-reasoning models never see the field.
	Reasoning bool         `json:"reasoning,omitempty"`
	Caps      Capabilities `json:"caps"` // protocol capabilities (per model: same protocol can differ by endpoint)
}

// StreamOptions carries per-request knobs. Zero values mean provider
// defaults. APIKey/BaseURL override config resolution (tests, relays).
type StreamOptions struct {
	MaxTokens     int
	Temperature   float64
	ThinkingLevel string         // "off"|"low"|"medium"|"high" as supported
	Cache         CacheRetention // empty = CacheShort
	APIKey        string
	BaseURL       string
	ExtraHeaders  map[string]string
	// PromptCacheKey routes implicit prefix caches (OpenAI-compatible
	// providers); providers clamp it to protocol limits.
	PromptCacheKey string
}

// Provider is a wire-protocol adapter: it turns a transcript into HTTP
// requests and a response into an event stream. Implementations must honor
// ctx cancellation (close the channel promptly) and follow the event
// contract documented on EventType.
type Provider interface {
	Name() string
	Caps() Capabilities
	// Stream starts one assistant turn. The returned channel is closed after
	// the terminal event. A non-nil error is reserved for setup failures
	// (unknown model, missing key); request and runtime failures arrive as
	// terminal error events.
	Stream(ctx context.Context, model Model, t *Transcript, opts StreamOptions) (<-chan Event, error)
}

// ResolveCacheRetention defaults empty to CacheShort.
func ResolveCacheRetention(r CacheRetention) CacheRetention {
	if r == "" {
		return CacheShort
	}
	return r
}
