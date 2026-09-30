// Package responses implements the OpenAI Responses API family of
// adapters (pi's openai-responses.ts / azure-openai-responses.ts with
// openai-responses-shared.ts): one wire protocol, two providers.
//
// The Responses API is item-based: the input array interleaves system
// prompts, user messages, and replayed assistant OUTPUT ITEMS
// (message / reasoning / function_call), and the stream emits typed
// events keyed by output slot. Multi-turn replay is stateless
// (store:false): reasoning items round-trip through the thinking block
// Signature as their verbatim JSON, and text blocks carry the message
// item id (and phase) in a v1 text signature.
//
// pi's grammar-constrained custom tools, additional_tools, and tool
// search items are compat-gated and default off; scode tools never
// declare constrainedSampling, so those paths are unreachable and not
// ported (noted in jindu.md's alignment report).
package responses

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"scode/internal/llm"
)

// minOutputTokens is the floor OpenAI Responses enforces on
// max_output_tokens (pi's OPENAI_RESPONSES_MIN_OUTPUT_TOKENS).
const minOutputTokens = 16

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

// inputText / inputImage are user- and tool-result-side content parts.
type inputText struct {
	Type string `json:"type"` // input_text
	Text string `json:"text"`
}

type inputImage struct {
	Type     string `json:"type"`      // input_image
	Detail   string `json:"detail"`    // "auto"
	ImageURL string `json:"image_url"` // data URL
}

// systemItem carries the (collapsed) system prompt; role is "developer"
// for reasoning models, "system" otherwise (pi's instructionRole).
type systemItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type userItem struct {
	Role    string `json:"role"` // user
	Content []any  `json:"content"`
}

type outputText struct {
	Type        string `json:"type"` // output_text
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// messageItem replays one assistant text block as a completed output
// message. ID keeps the server-issued msg_* id stable across turns;
// Phase carries OpenAI's commentary/final_answer marker when present.
type messageItem struct {
	Type    string       `json:"type"` // message
	Role    string       `json:"role"` // assistant
	Content []outputText `json:"content"`
	Status  string       `json:"status"` // completed
	ID      string       `json:"id"`
	Phase   string       `json:"phase,omitempty"`
}

// reasoningItem replays verbatim (the stored Signature IS the item's
// JSON), so it marshals as raw.
type reasoningItem json.RawMessage

// MarshalJSON emits the stored item JSON verbatim. The type definition
// does not inherit json.RawMessage's method set, and without this
// override encoding/json would serialize the byte slice as a base64
// string — breaking every stateless replay turn.
func (r reasoningItem) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

type functionCallItem struct {
	Type      string `json:"type"` // function_call
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string, not an object
}

type functionCallOutputItem struct {
	Type   string `json:"type"` // function_call_output
	CallID string `json:"call_id"`
	Output any    `json:"output"` // string or []any of inputText/inputImage
}

type wireFunctionTool struct {
	Type        string          `json:"type"` // function
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"` // emitted when the provider supports strict mode
}

type wireReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

type requestParams struct {
	Model                string             `json:"model"`
	Input                []any              `json:"input"`
	Stream               bool               `json:"stream"`
	Store                bool               `json:"store"`
	PromptCacheKey       string             `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string             `json:"prompt_cache_retention,omitempty"` // "24h" (long retention)
	MaxOutputTokens      int                `json:"max_output_tokens,omitempty"`
	Temperature          *float64           `json:"temperature,omitempty"`
	Tools                []wireFunctionTool `json:"tools,omitempty"`
	Reasoning            *wireReasoning     `json:"reasoning,omitempty"`
	Include              []string           `json:"include,omitempty"`
}

// ---------------------------------------------------------------------------
// Text signatures (pi's TextSignatureV1): text blocks carry the message
// item id (and phase) so replay pairs with server-side reasoning items.
// ---------------------------------------------------------------------------

type textSignatureV1 struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Phase string `json:"phase,omitempty"` // commentary | final_answer
}

func encodeTextSignature(id, phase string) string {
	b, _ := json.Marshal(textSignatureV1{V: 1, ID: id, Phase: phase})
	return string(b)
}

// parseTextSignature accepts the v1 JSON form and legacy plain strings
// (pi's parseTextSignature).
func parseTextSignature(sig string) (id, phase string, ok bool) {
	if sig == "" {
		return "", "", false
	}
	if strings.HasPrefix(sig, "{") {
		var v textSignatureV1
		if err := json.Unmarshal([]byte(sig), &v); err == nil && v.V == 1 && v.ID != "" {
			if v.Phase == "commentary" || v.Phase == "final_answer" {
				return v.ID, v.Phase, true
			}
			return v.ID, "", true
		}
		// fall through to legacy plain-string handling
	}
	return sig, "", true
}

// ---------------------------------------------------------------------------
// Tool call ID normalization (pi's convertResponsesMessages helpers)
// ---------------------------------------------------------------------------

var idPartCleaner = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// normalizeIDPart restricts an id segment to the OpenAI Responses id
// grammar, max 64 chars, trailing underscores stripped.
func normalizeIDPart(part string) string {
	s := idPartCleaner.ReplaceAllString(part, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.TrimRight(s, "_")
}

// foreignItemID hashes a foreign provider's item id into the fc_* space.
func foreignItemID(itemID string) string {
	s := "fc_" + llm.ShortHash(itemID)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// normalizeToolCallID rewrites composite "callId|itemId" ids for replay
// against a Responses endpoint (pi's normalizeToolCallId). allowed is the
// provider set whose ids the target accepts verbatim (OPENAI_TOOL_CALL_PROVIDERS
// / AZURE_TOOL_CALL_PROVIDERS).
func normalizeToolCallID(id, targetProvider string, allowed map[string]bool, source llm.Message) string {
	if !allowed[targetProvider] {
		return normalizeIDPart(id)
	}
	if !strings.Contains(id, "|") {
		return normalizeIDPart(id)
	}
	callID, itemID, _ := strings.Cut(id, "|")
	callID = normalizeIDPart(callID)
	foreign := source.Provider != targetProvider // provider implies api shape in scode's registry
	if foreign {
		itemID = foreignItemID(itemID)
	} else {
		itemID = normalizeIDPart(itemID)
	}
	// The Responses API requires item ids to start with "fc".
	if !strings.HasPrefix(itemID, "fc_") {
		itemID = normalizeIDPart("fc_" + itemID)
	}
	return callID + "|" + itemID
}

// ---------------------------------------------------------------------------
// Message conversion (pi's convertResponsesMessages, default compat path:
// no mid-conversation system messages, no grammar tools, no additional
// tools / tool search)
// ---------------------------------------------------------------------------

// convertMessages builds the Responses input array. By default system
// messages collapse into the leading developer/system item (later
// deltas fold in via CurrentSystemMessage); with
// Caps.MidConvoSystem (pi's compat.supportsMidConvoSystemMessages)
// the head is the leading message alone and later deltas ride in
// place, keeping the request head cache-stable. The transform
// normalizes cross-model tool call ids and answers orphaned calls.
func convertMessages(model llm.Model, msgs []llm.Message, allowed map[string]bool) []any {
	norm := func(id string, source llm.Message) string {
		if source.Provider == model.Provider && source.Model == model.ID {
			return id // server-issued ids replay verbatim (pi's isSameModel gate)
		}
		return normalizeToolCallID(id, model.Provider, allowed, source)
	}
	midConvo := model.Caps.MidConvoSystem
	// Collapse system messages first (compat.supportsMidConvoSystemMessages
	// defaults false), then transform the conversation (pi's order).
	var convo []llm.Message
	for i, m := range msgs {
		if m.Role == llm.RoleSystem && (i == 0 || !midConvo) {
			continue // leading renders as the head; deltas collapse (default)
		}
		convo = append(convo, m)
	}
	convo = llm.TransformMessagesIDs(convo, norm)

	sysRole := func() string {
		if model.Reasoning { // compat.supportsDeveloperRole defaults true
			return "developer"
		}
		return "system"
	}
	var out []any
	if midConvo {
		if text := renderSystem(msgs[0]); text != "" {
			out = append(out, systemItem{Role: sysRole(), Content: llm.Sanitize(text)})
		}
	} else if sys := llm.CurrentSystemMessage(msgs); sys != nil {
		if text := renderSystem(*sys); text != "" {
			out = append(out, systemItem{Role: sysRole(), Content: llm.Sanitize(text)})
		}
	}

	for msgIndex, m := range convo {
		switch m.Role {
		case llm.RoleSystem:
			// Mid-conversation delta riding in place (midConvo path only —
			// convo carries no system messages otherwise).
			if text := renderSystem(m); text != "" {
				out = append(out, systemItem{Role: sysRole(), Content: llm.Sanitize(text)})
			}
		case llm.RoleUser:
			var content []any
			prevPlaceholder := false
			for _, b := range m.Content {
				switch b.Kind {
				case llm.BlockText:
					content = append(content, inputText{Type: "input_text", Text: llm.Sanitize(b.Text)})
					prevPlaceholder = false
				case llm.BlockImage:
					if !model.Caps.ImageInput {
						// pi's downgradeUnsupportedImages: one placeholder
						// per run of images.
						if !prevPlaceholder {
							content = append(content, inputText{Type: "input_text", Text: nonVisionUserImagePlaceholder})
							prevPlaceholder = true
						}
						continue
					}
					content = append(content, inputImage{
						Type:     "input_image",
						Detail:   "auto",
						ImageURL: "data:" + b.MimeType + ";base64," + b.Data,
					})
					prevPlaceholder = false
				}
			}
			if len(content) == 0 {
				continue
			}
			out = append(out, userItem{Role: "user", Content: content})

		case llm.RoleAssistant:
			out = append(out, convertAssistant(model, m, msgIndex)...)

		case llm.RoleTool:
			for _, b := range m.Content {
				if b.Kind != llm.BlockToolResult {
					continue
				}
				callID, _, _ := strings.Cut(b.ID, "|")
				out = append(out, functionCallOutputItem{
					Type:   "function_call_output",
					CallID: callID,
					Output: convertToolResultOutput(model, b),
				})
			}
		}
	}
	return out
}

const (
	nonVisionUserImagePlaceholder = "(image omitted: model does not support images)"
	nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"
)

// convertAssistant replays one assistant message as output items
// (reasoning / message / function_call), mirroring pi's per-block rules.
func convertAssistant(model llm.Model, m llm.Message, msgIndex int) []any {
	sameProvider := m.Provider == model.Provider
	sameModel := sameProvider && m.Model == model.ID
	differentModel := sameProvider && m.Model != model.ID

	var out []any
	textBlockIndex := 0
	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockThinking:
			// Redacted thinking is Anthropic-specific opaque content; the
			// Responses API has no slot for it (pi drops it cross-model,
			// and same-model redacted cannot occur here).
			if b.Redacted {
				continue
			}
			if sameModel {
				// Reasoning replays ONLY via the stored item JSON; unsigned
				// thinking is dropped (pi: no thinkingSignature → no item).
				if b.Signature != "" {
					out = append(out, reasoningItem(json.RawMessage(b.Signature)))
				}
				continue
			}
			// Cross-provider/model thinking lowers to plain assistant text
			// (pi's transformMessages), then flows through the text path.
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			out = append(out, buildMessageItem(b.Text, "", msgIndex, &textBlockIndex))

		case llm.BlockText:
			out = append(out, buildMessageItem(b.Text, b.Signature, msgIndex, &textBlockIndex))

		case llm.BlockToolCall:
			callID, itemID, _ := strings.Cut(b.ID, "|")
			// Drop the item id when it cannot pair server-side: different
			// model (fc_ pairing validation) or a non-fc_ id (pi's rules;
			// the grammar custom-tool branch is unreachable in scode).
			if itemID != "" && ((differentModel && strings.HasPrefix(itemID, "fc_")) || !strings.HasPrefix(itemID, "fc_")) {
				itemID = ""
			}
			out = append(out, functionCallItem{
				Type:      "function_call",
				ID:        itemID,
				CallID:    callID,
				Name:      b.Name,
				Arguments: compactArguments(b.Arguments),
			})
		}
	}
	return out
}

// buildMessageItem emits one completed assistant output message, keeping
// the server-issued id when the text signature carries one (pi's text
// block handling, including the msg_pi_* fallback ids).
func buildMessageItem(text, signature string, msgIndex int, textBlockIndex *int) messageItem {
	id, phase, _ := parseTextSignature(signature)
	if id == "" {
		if *textBlockIndex == 0 {
			id = fmt.Sprintf("msg_pi_%d", msgIndex)
		} else {
			id = fmt.Sprintf("msg_pi_%d_%d", msgIndex, *textBlockIndex)
		}
	}
	*textBlockIndex++
	if len(id) > 64 { // OpenAI caps item ids at 64 characters
		id = "msg_" + llm.ShortHash(id)
	}
	return messageItem{
		Type:    "message",
		Role:    "assistant",
		Content: []outputText{{Type: "output_text", Text: llm.Sanitize(text), Annotations: []any{}}},
		Status:  "completed",
		ID:      id,
		Phase:   phase,
	}
}

// convertToolResultOutput renders a tool result as plain text or, for
// vision models, a text+image content array (pi's convertToolResultOutput).
func convertToolResultOutput(model llm.Model, b llm.Block) any {
	var texts []string
	var images []llm.Block
	prevPlaceholder := false
	for _, nb := range b.Content {
		switch nb.Kind {
		case llm.BlockText:
			texts = append(texts, nb.Text)
			prevPlaceholder = false
		case llm.BlockImage:
			if !model.Caps.ImageInput {
				// pi's transform downgrade replaces an image run with one
				// placeholder text block.
				if !prevPlaceholder {
					texts = append(texts, nonVisionToolImagePlaceholder)
					prevPlaceholder = true
				}
				continue
			}
			images = append(images, nb)
			prevPlaceholder = false
		}
	}
	// A flat Text payload on the block itself counts as text (the agent
	// stores simple results that way).
	if b.Text != "" {
		texts = append(texts, b.Text)
	}
	text := strings.Join(texts, "\n")

	if len(images) == 0 {
		if text != "" {
			return llm.Sanitize(text)
		}
		return "(no tool output)"
	}
	// Vision models: text part only when text exists (pi adds no
	// placeholder here), then one input_image per image.
	var content []any
	if text != "" {
		content = append(content, inputText{Type: "input_text", Text: llm.Sanitize(text)})
	}
	for _, im := range images {
		content = append(content, inputImage{
			Type:     "input_image",
			Detail:   "auto",
			ImageURL: "data:" + im.MimeType + ";base64," + im.Data,
		})
	}
	return content
}

// convertTools builds the function tool declarations. strict is emitted
// only when the provider supports strict mode (azure default true →
// strict:false; openai default false → omitted). scode tools never
// declare constrainedSampling, so pi's strict-schema upgrade and grammar
// custom tools do not apply.
func convertTools(tools []llm.Tool, supportsStrictMode bool) []wireFunctionTool {
	var out []wireFunctionTool
	for _, t := range tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		wt := wireFunctionTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		}
		if supportsStrictMode {
			strict := false // pi's defaultStrict
			wt.Strict = &strict
		}
		out = append(out, wt)
	}
	return out
}

// compactArguments renders stored tool-call arguments as the wire's JSON
// string field, compacting valid JSON and falling back to {} for corrupt
// fragments (pi's JSON.stringify(parseStreamingJson(...)) guarantees an
// object; compactArgs semantics).
func compactArguments(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// renderSystem produces the deterministic system text: base content first,
// then each replayed section wrapped in <name> tags (pi's section shape —
// the coding agent pre-wraps section values in tags before they reach
// SystemMessage.sections).
func renderSystem(m llm.Message) string {
	base := ""
	for _, b := range m.Content {
		if b.Kind == llm.BlockText {
			if base != "" {
				base += "\n"
			}
			base += b.Text
		}
	}
	out := base
	for _, s := range m.Sections {
		if out != "" {
			out += "\n\n"
		}
		out += "<" + s.Name + ">\n" + s.Value + "\n</" + s.Name + ">"
	}
	return out
}
