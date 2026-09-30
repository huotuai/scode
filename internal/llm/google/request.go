package google

import (
	"encoding/json"
	"regexp"
	"strings"

	"scode/internal/llm"
)

// ---------------------------------------------------------------------------
// Wire shapes (Gemini v1beta generateContent)
// ---------------------------------------------------------------------------

// part is one Content part. thoughtSignature is a sibling of text and
// functionCall (pi's Part handling); inlineData carries images.
type part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   string          `json:"id,omitempty"` // gemini >= 3 / claude-* / gpt-oss-* only
}

type functionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`        // {"output": ...} | {"error": ...}
	ID       string          `json:"id,omitempty"`    // same gate as functionCall.id
	Parts    []part          `json:"parts,omitempty"` // multimodal function responses (gemini >= 3)
}

type content struct {
	Role  string `json:"role,omitempty"` // user | model; empty on systemInstruction
	Parts []part `json:"parts"`
}

type functionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type toolDecl struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type thinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`  // MINIMAL | LOW | MEDIUM | HIGH (level models)
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"` // token budget models; -1 = dynamic, 0 = off
}

type generationConfig struct {
	Temperature     *float64        `json:"temperature,omitempty"`
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type wireRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Tools             []toolDecl        `json:"tools,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

// ---------------------------------------------------------------------------
// Model-shape predicates (pi's google-shared.ts)
// ---------------------------------------------------------------------------

var (
	geminiMajorRe     = regexp.MustCompile(`^gemini(?:-live)?-(\d+)`)
	gemini3LevelRe    = regexp.MustCompile(`gemini-3(?:\.\d+)?-(?:pro|flash)`)
	gemma4Re          = regexp.MustCompile(`gemma-?4`)
	signatureBase64Re = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
)

// usesThinkingLevel selects Gemini's discrete thinkingLevel control over
// the token-based thinkingBudget (pi's usesGoogleThinkingLevel: Gemini 3
// Pro/Flash, the flash-latest aliases, and Gemma 4).
func usesThinkingLevel(modelID string) bool {
	id := strings.ToLower(modelID)
	return gemini3LevelRe.MatchString(id) ||
		id == "gemini-flash-latest" ||
		id == "gemini-flash-lite-latest" ||
		gemma4Re.MatchString(id)
}

// requiresToolCallID: models behind Google APIs that require explicit
// tool call ids in function calls/responses (pi's requiresToolCallId).
func requiresToolCallID(modelID string) bool {
	id := strings.ToLower(modelID)
	if strings.HasPrefix(id, "claude-") || strings.HasPrefix(id, "gpt-oss-") {
		return true
	}
	major, ok := geminiMajorVersion(id)
	return ok && major >= 3
}

func geminiMajorVersion(modelID string) (int, bool) {
	m := geminiMajorRe.FindStringSubmatch(strings.ToLower(modelID))
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n, true
}

// supportsMultimodalFunctionResponse: Gemini 3+ accepts images nested in
// functionResponse.parts; other models need a separate image turn (pi).
func supportsMultimodalFunctionResponse(modelID string) bool {
	major, ok := geminiMajorVersion(modelID)
	if !ok {
		return true
	}
	return major >= 3
}

// validThoughtSignature: thought signatures must be base64 (TYPE_BYTES).
func validThoughtSignature(sig string) bool {
	return sig != "" && len(sig)%4 == 0 && signatureBase64Re.MatchString(sig)
}

// resolveThoughtSignature keeps signatures only from the same
// provider/model with valid base64 (pi's resolveThoughtSignature).
func resolveThoughtSignature(sameProviderAndModel bool, sig string) string {
	if sameProviderAndModel && validThoughtSignature(sig) {
		return sig
	}
	return ""
}

// ---------------------------------------------------------------------------
// Thinking config (pi's thinking level → thinkingConfig mapping)
// ---------------------------------------------------------------------------

// budgetFor returns the thinking budget for budget-style models
// (pi's getGoogleBudget tables); -1 means dynamic.
func budgetFor(modelID, level string) int {
	if strings.Contains(modelID, "2.5-pro") {
		return map[string]int{"minimal": 128, "low": 2048, "medium": 8192, "high": 32768}[level]
	}
	// flash-lite must match before flash (pi's ordering: the lite id
	// contains the flash prefix).
	if strings.Contains(modelID, "2.5-flash-lite") {
		return map[string]int{"minimal": 512, "low": 2048, "medium": 8192, "high": 24576}[level]
	}
	if strings.Contains(modelID, "2.5-flash") {
		return map[string]int{"minimal": 128, "low": 2048, "medium": 8192, "high": 24576}[level]
	}
	return -1
}

// clampLevel maps scode thinking levels onto Google's resolved levels
// (pi's clampThinkingLevel with no per-model map: xhigh/max fall back to
// the nearest supported level, which is high).
func clampLevel(level string) string {
	switch level {
	case "minimal", "low", "medium", "high":
		return level
	default: // xhigh, max, unknown
		return "high"
	}
}

// resolveThinkingConfig builds the thinkingConfig for reasoning models
// (pi's buildParams thinking block + getDisabledGoogleThinkingConfig
// without a per-model thinkingLevelMap, which always resolves to
// budget-0 disabled).
func resolveThinkingConfig(model llm.Model, level string) *thinkingConfig {
	if !model.Reasoning {
		return nil
	}
	if level == "" || level == "off" {
		zero := 0
		return &thinkingConfig{ThinkingBudget: &zero}
	}
	l := clampLevel(level)
	if usesThinkingLevel(model.ID) {
		return &thinkingConfig{IncludeThoughts: true, ThinkingLevel: strings.ToUpper(l)}
	}
	budget := budgetFor(model.ID, l)
	return &thinkingConfig{IncludeThoughts: true, ThinkingBudget: &budget}
}

// ---------------------------------------------------------------------------
// Message conversion (pi's google-shared convertMessages)
// ---------------------------------------------------------------------------

// BuildRequest converts a transcript into a streamGenerateContent body.
// The collapsed system prompt rides systemInstruction; tool results merge
// into a single user turn (Cloud Code Assist requires it); the request
// transform normalizes cross-model tool call ids for id-requiring models.
func BuildRequest(model llm.Model, t *llm.Transcript, opts llm.StreamOptions) *wireRequest {
	msgs := t.Messages()

	var convo []llm.Message
	for _, m := range msgs {
		if m.Role != llm.RoleSystem {
			convo = append(convo, m)
		}
	}
	needID := requiresToolCallID(model.ID)
	norm := func(id string, source llm.Message) string {
		if source.Provider == model.Provider && source.Model == model.ID {
			return id // pi's transformMessages gates normalization on !isSameModel
		}
		if !needID {
			return id
		}
		return normalizeToolCallID(id)
	}
	convo = llm.TransformMessagesIDs(convo, norm)

	// Tool names are not stored on older sessions' toolResult blocks;
	// index the call ids so functionResponse can name its tool (pi's
	// ToolResultMessage.toolName). Prefer the block's own Name.
	toolNames := map[string]string{}
	for _, m := range convo {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolCall {
				toolNames[b.ID] = b.Name
			}
		}
	}

	req := &wireRequest{Contents: convertMessages(model, convo, toolNames)}

	if sys := llm.CurrentSystemMessage(msgs); sys != nil {
		if text := renderSystem(*sys); text != "" {
			req.SystemInstruction = &content{Parts: []part{{Text: llm.Sanitize(text)}}}
		}
	}

	if tools := llm.CurrentTools(msgs); len(tools) > 0 {
		var decls []functionDeclaration
		for _, tl := range tools {
			params := tl.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			decls = append(decls, functionDeclaration{
				Name:                 tl.Name,
				Description:          tl.Description,
				ParametersJSONSchema: params,
			})
		}
		req.Tools = []toolDecl{{FunctionDeclarations: decls}}
		// toolConfig.functionCallingConfig only applies with a toolChoice
		// (scode has none) or strict-sampling tools (unreachable), so it
		// stays unset (pi's resolveGoogleFunctionCallingMode default).
	}

	gc := &generationConfig{}
	if opts.Temperature != 0 {
		gc.Temperature = &opts.Temperature
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = model.MaxTokens
	}
	if maxTokens > 0 {
		gc.MaxOutputTokens = maxTokens
	}
	gc.ThinkingConfig = resolveThinkingConfig(model, opts.ThinkingLevel)
	if gc.Temperature != nil || gc.MaxOutputTokens != 0 || gc.ThinkingConfig != nil {
		req.GenerationConfig = gc
	}
	return req
}

// normalizeToolCallID restricts ids to Gemini's grammar (pi's google
// normalizeToolCallId): [a-zA-Z0-9_-], max 64 chars.
func normalizeToolCallID(id string) string {
	s := idPartCleaner.ReplaceAllString(id, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

var idPartCleaner = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func convertMessages(model llm.Model, convo []llm.Message, toolNames map[string]string) []content {
	var contents []content
	needID := requiresToolCallID(model.ID)
	multimodalResponse := supportsMultimodalFunctionResponse(model.ID)

	for _, m := range convo {
		switch m.Role {
		case llm.RoleUser:
			var parts []part
			prevPlaceholder := false
			for _, b := range m.Content {
				switch b.Kind {
				case llm.BlockText:
					parts = append(parts, part{Text: llm.Sanitize(b.Text)})
					prevPlaceholder = false
				case llm.BlockImage:
					if !model.Caps.ImageInput {
						// pi's downgradeUnsupportedImages placeholder.
						if !prevPlaceholder {
							parts = append(parts, part{Text: "(image omitted: model does not support images)"})
							prevPlaceholder = true
						}
						continue
					}
					parts = append(parts, part{InlineData: &inlineData{MimeType: b.MimeType, Data: b.Data}})
					prevPlaceholder = false
				}
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, content{Role: "user", Parts: parts})

		case llm.RoleAssistant:
			same := m.Provider == model.Provider && m.Model == model.ID
			var parts []part
			for _, b := range m.Content {
				switch b.Kind {
				case llm.BlockText:
					sig := resolveThoughtSignature(same, b.Signature)
					// Skip empty text unless it carries a signature Gemini
					// requires echoed (pi's reasoning-chain rule).
					if strings.TrimSpace(b.Text) == "" && sig == "" {
						continue
					}
					parts = append(parts, part{Text: llm.Sanitize(b.Text), ThoughtSignature: sig})
				case llm.BlockThinking:
					if b.Redacted {
						continue // Anthropic-specific opaque content; Gemini has no slot
					}
					if same {
						sig := resolveThoughtSignature(same, b.Signature)
						if strings.TrimSpace(b.Text) == "" && sig == "" {
							continue
						}
						parts = append(parts, part{Thought: true, Text: llm.Sanitize(b.Text), ThoughtSignature: sig})
					} else {
						// Cross-provider/model: lower to plain text (pi).
						if strings.TrimSpace(b.Text) == "" {
							continue
						}
						parts = append(parts, part{Text: llm.Sanitize(b.Text)})
					}
				case llm.BlockToolCall:
					args := b.Arguments
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					fc := &functionCall{Name: b.Name, Args: args}
					if needID {
						fc.ID = b.ID
					}
					parts = append(parts, part{
						FunctionCall:     fc,
						ThoughtSignature: resolveThoughtSignature(same, b.Signature),
					})
				}
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, content{Role: "model", Parts: parts})

		case llm.RoleTool:
			for _, b := range m.Content {
				if b.Kind != llm.BlockToolResult {
					continue
				}
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
							if !prevPlaceholder {
								texts = append(texts, "(tool image omitted: model does not support images)")
								prevPlaceholder = true
							}
							continue
						}
						images = append(images, nb)
						prevPlaceholder = false
					}
				}
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
				text := strings.Join(texts, "\n")
				// "output" on success, "error" on failure (SDK convention).
				value := text
				if value == "" && len(images) > 0 {
					value = "(see attached image)"
				}
				respKey := "output"
				if b.IsError {
					respKey = "error"
				}
				respBody, _ := json.Marshal(map[string]string{respKey: llm.Sanitize(value)})

				name := b.Name
				if name == "" {
					name = toolNames[b.ID] // legacy sessions without toolName
				}
				fr := &functionResponse{Name: name, Response: respBody}
				if len(images) > 0 && multimodalResponse {
					for _, im := range images {
						fr.Parts = append(fr.Parts, part{InlineData: &inlineData{MimeType: im.MimeType, Data: im.Data}})
					}
				}
				if needID {
					fr.ID = b.ID
				}
				fpart := part{FunctionResponse: fr}

				// Cloud Code Assist requires all function responses in a
				// single user turn: merge into a trailing user content
				// that already carries function responses (pi's rule).
				if n := len(contents); n > 0 && contents[n-1].Role == "user" && hasFunctionResponse(contents[n-1]) {
					contents[n-1].Parts = append(contents[n-1].Parts, fpart)
				} else {
					contents = append(contents, content{Role: "user", Parts: []part{fpart}})
				}

				// Gemini < 3: images ride a separate user message (pi).
				if len(images) > 0 && !multimodalResponse {
					imgParts := []part{{Text: "Tool result image:"}}
					for _, im := range images {
						imgParts = append(imgParts, part{InlineData: &inlineData{MimeType: im.MimeType, Data: im.Data}})
					}
					contents = append(contents, content{Role: "user", Parts: imgParts})
				}
			}
		}
	}
	return contents
}

func hasFunctionResponse(c content) bool {
	for _, p := range c.Parts {
		if p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// renderSystem matches the other adapters' section rendering so the same
// transcript produces equivalent prompts across protocols.
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
