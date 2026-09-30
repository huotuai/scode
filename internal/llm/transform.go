package llm

// IDNormalizer rewrites a tool call ID for the target provider
// (pi's transformMessages normalizeToolCallId hook). source is the
// assistant message the call came from, so the normalizer can skip
// same-provider/same-model ids (their server-issued ids replay
// verbatim). Returning the id unchanged leaves it (and its tool
// results) untouched.
type IDNormalizer func(id string, source Message) string

// TransformMessagesIDs is TransformMessages plus pi's tool-call ID
// normalization pass: assistant toolCall IDs pass through norm, the
// old→new mapping rewrites matching toolResult IDs, and the rewritten
// stream feeds the standard transform (so synthetic orphan results
// carry the normalized IDs). Used by protocols with strict ID grammars
// (OpenAI Responses fc_* item ids, Bedrock's 64-char toolUseId).
func TransformMessagesIDs(msgs []Message, norm IDNormalizer) []Message {
	if norm == nil {
		return TransformMessages(msgs)
	}
	idMap := map[string]string{}
	rewritten := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			var content []Block
			changed := false
			for _, b := range m.Content {
				if b.Kind == BlockToolCall && b.ID != "" {
					if nid := norm(b.ID, m); nid != b.ID {
						idMap[b.ID] = nid
						b.ID = nid
						changed = true
					}
				}
				content = append(content, b)
			}
			if changed {
				m.Content = content
			}
		case RoleTool:
			var content []Block
			changed := false
			for _, b := range m.Content {
				if b.Kind == BlockToolResult {
					if nid, ok := idMap[b.ID]; ok {
						b.ID = nid
						changed = true
					}
				}
				content = append(content, b)
			}
			if changed {
				m.Content = content
			}
		}
		rewritten = append(rewritten, m)
	}
	return TransformMessages(rewritten)
}

// TransformMessages prepares transcript messages for a provider request
// (pi's transform-messages.ts):
//
//   - Errored/aborted assistant messages are skipped entirely. They are
//     incomplete turns: replaying them can cause API errors, and the
//     model should retry from the last valid state.
//   - Tool calls without a matching result receive a synthetic error
//     toolResult IN PLACE ("No result provided"), so one interrupted
//     turn cannot brick every later request.
//   - A system message landing between a call and its results is held
//     back and emitted after the (synthetic) results, so it never
//     causes a duplicate result for a call answered later.
//
// The transform is request-time only; the stored transcript is never
// touched.
func TransformMessages(msgs []Message) []Message {
	var result []Message
	var pending []Block // tool calls awaiting a result
	answered := map[string]bool{}
	var held []Message

	closePending := func() {
		if len(pending) > 0 {
			for _, tc := range pending {
				if !answered[tc.ID] {
					result = append(result, Message{
						Role: RoleTool,
						Content: []Block{{
							Kind:    BlockToolResult,
							ID:      tc.ID,
							Content: []Block{TextBlock("No result provided")},
							IsError: true,
						}},
					})
				}
			}
			pending = nil
			answered = map[string]bool{}
		}
		result = append(result, held...)
		held = nil
	}

	for _, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			closePending()
			if m.StopReason == StopError || m.StopReason == StopAborted {
				continue
			}
			var calls []Block
			for _, b := range m.Content {
				if b.Kind == BlockToolCall {
					calls = append(calls, b)
				}
			}
			if len(calls) > 0 {
				pending = calls
				answered = map[string]bool{}
			}
			result = append(result, m)
		case RoleTool:
			m.Content = sanitizeImages(m.Content)
			for _, b := range m.Content {
				if b.Kind == BlockToolResult {
					answered[b.ID] = true
				}
			}
			result = append(result, m)
		case RoleSystem:
			if len(pending) > 0 {
				held = append(held, m)
			} else {
				result = append(result, m)
			}
		default: // user and anything else interrupts the tool flow
			closePending()
			m.Content = sanitizeImages(m.Content)
			result = append(result, m)
		}
	}
	closePending()
	return limitReplayedImages(result)
}

// keepRecentImageMessages bounds how many image-BEARING messages ride
// each request. Images never leave the stored transcript, but only the
// newest few replay; older ones downgrade to a text placeholder at
// request time (the same request-time-only discipline as
// sanitizeImages). A screenshot from thirty turns ago almost never
// matters to the current task, yet every replay spends context window
// on every turn and pays full retransmission on every cache miss.
// Counting is tail-anchored, so an old message's downgrade state flips
// at most once per eviction — the prefix changes once there instead of
// churning per request.
const keepRecentImageMessages = 10

// oldImagePlaceholder replaces an image block that aged out of the
// replay window.
const oldImagePlaceholder = "[image omitted: older image kept out of the replay to save context]"

// limitReplayedImages downgrades image blocks in all but the newest
// keepRecentImageMessages image-bearing messages. The input is already
// a request-time copy; messages are only rewritten when something
// actually changes.
func limitReplayedImages(msgs []Message) []Message {
	quota := keepRecentImageMessages
	var out []Message // lazily copied on the first downgrade
	for i := len(msgs) - 1; i >= 0; i-- {
		if !messageHasImage(msgs[i]) {
			continue
		}
		if quota > 0 {
			quota--
			continue
		}
		if out == nil {
			out = make([]Message, len(msgs))
			copy(out, msgs)
		}
		if c, changed := downgradeImagesIn(out[i].Content); changed {
			out[i].Content = c
		}
	}
	if out == nil {
		return msgs
	}
	return out
}

// messageHasImage reports whether a message carries an image block,
// descending into toolResult contents (where read-tool images live).
func messageHasImage(m Message) bool {
	return hasImageIn(m.Content)
}

func hasImageIn(content []Block) bool {
	for _, b := range content {
		switch b.Kind {
		case BlockImage:
			return true
		case BlockToolResult:
			if hasImageIn(b.Content) {
				return true
			}
		}
	}
	return false
}

// downgradeImagesIn replaces every image block with the aged-out
// placeholder, descending into toolResult contents. The stored
// transcript is never touched — this runs on request-time copies.
func downgradeImagesIn(content []Block) ([]Block, bool) {
	if len(content) == 0 {
		return content, false
	}
	out := make([]Block, len(content))
	copy(out, content)
	changed := false
	for i := range out {
		switch out[i].Kind {
		case BlockImage:
			out[i] = TextBlock(oldImagePlaceholder)
			changed = true
		case BlockToolResult:
			if nested, c := downgradeImagesIn(out[i].Content); c {
				out[i].Content = nested
				changed = true
			}
		}
	}
	return out, changed
}

// emptyImagePlaceholder replaces an image block that carries no data.
const emptyImagePlaceholder = "[image omitted: empty image data]"

// sanitizeImages rewrites image blocks that cannot travel on any wire
// (empty payload) into text placeholders. A tool that read a zero-byte or
// mislabeled file — or an MCP server that returned a broken image — would
// otherwise persist an image block that every later request replays;
// provider gateways reject the whole request over one bad image (Kimi
// reports empty bytes as "unsupported image format: text/plain;
// charset=utf-8"), so a single block bricks the session until it is
// removed. Running here, at request time, also repairs transcripts that
// already contain such a block. The stored transcript is never touched.
func sanitizeImages(content []Block) []Block {
	out, changed := sanitizeImagesIn(content)
	if !changed {
		return content
	}
	return out
}

// sanitizeImagesIn walks one content slice, descending into toolResult
// blocks (where read images live), and reports whether anything changed.
func sanitizeImagesIn(content []Block) ([]Block, bool) {
	if len(content) == 0 {
		return content, false
	}
	out := make([]Block, len(content))
	copy(out, content)
	changed := false
	for i := range out {
		switch out[i].Kind {
		case BlockImage:
			if out[i].Data == "" {
				out[i] = TextBlock(emptyImagePlaceholder)
				changed = true
			}
		case BlockToolResult:
			if nested, c := sanitizeImagesIn(out[i].Content); c {
				out[i].Content = nested
				changed = true
			}
		}
	}
	return out, changed
}
