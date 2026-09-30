package llm

import (
	"testing"
)

func xformUserMsg(text string) Message {
	return Message{Role: RoleUser, Content: []Block{TextBlock(text)}}
}

func asstMsg(stop StopReason, blocks ...Block) Message {
	return Message{Role: RoleAssistant, StopReason: stop, Content: blocks}
}

func toolMsg(id, text string) Message {
	return Message{Role: RoleTool, Content: []Block{
		{Kind: BlockToolResult, ID: id, Content: []Block{TextBlock(text)}},
	}}
}

// pi's transform-messages: errored/aborted assistant turns are dropped;
// orphaned tool calls get an in-place synthetic error result.
func TestTransformMessagesSkipsErrorTurns(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: []Block{TextBlock("sys")}},
		xformUserMsg("go"),
		asstMsg(StopError, TextBlock("partial")),
		xformUserMsg("again"),
		asstMsg(StopEndTurn, TextBlock("ok")),
	}
	out := TransformMessages(msgs)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 (error turn dropped): %+v", len(out), out)
	}
	for _, m := range out {
		if m.StopReason == StopError {
			t.Fatalf("error turn leaked: %+v", m)
		}
	}
	if out[2].Content[0].Text != "again" || out[3].Content[0].Text != "ok" {
		t.Fatalf("order wrong: %+v", out[2:])
	}
}

func TestTransformMessagesOrphanSynthesisInPlace(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: []Block{TextBlock("sys")}},
		xformUserMsg("go"),
		asstMsg(StopToolUse,
			ToolCallBlock("answered", "read"),
			ToolCallBlock("orphan", "bash"),
		),
		toolMsg("answered", "file contents"),
		xformUserMsg("next question"),
	}
	out := TransformMessages(msgs)
	// sys, user, assistant, tool(answered), tool(synthetic orphan), user
	if len(out) != 6 {
		t.Fatalf("len = %d, want 6: %+v", len(out), out)
	}
	synth := out[4]
	if synth.Role != RoleTool || len(synth.Content) != 1 {
		t.Fatalf("synthetic message = %+v", synth)
	}
	b := synth.Content[0]
	if b.ID != "orphan" || !b.IsError || b.Content[0].Text != "No result provided" {
		t.Fatalf("synthetic result = %+v", b)
	}
	// In place: directly after the real result, before the next user turn.
	if out[5].Role != RoleUser {
		t.Fatalf("synthesis not in place: %+v", out[5])
	}
}

func TestTransformMessagesSystemHeldBehindResults(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: []Block{TextBlock("sys")}},
		xformUserMsg("go"),
		asstMsg(StopToolUse, ToolCallBlock("c1", "read")),
		{Role: RoleSystem, Sections: []Section{{Name: "s", Value: "v"}}},
		xformUserMsg("interrupt"),
	}
	out := TransformMessages(msgs)
	// The delta system message must come AFTER the synthetic result,
	// not between the call and its answer.
	var roles []Role
	for _, m := range out {
		roles = append(roles, m.Role)
	}
	// sys, user, assistant, tool(synthetic), system(held), user
	want := []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool, RoleSystem, RoleUser}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
}

// An empty image block (a read of a zero-byte screenshot) must be
// rewritten to text at request time; otherwise every provider request
// replays it and Kimi rejects the whole session. Valid images stay.
func TestTransformMessagesDropsEmptyImages(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []Block{
			TextBlock("look"),
			{Kind: BlockImage, MimeType: "image/png", Data: ""},
			{Kind: BlockImage, MimeType: "image/png", Data: "aGk="},
		}},
		{Role: RoleTool, Content: []Block{{
			Kind: BlockToolResult, ID: "t1",
			Content: []Block{TextBlock("shot"), {Kind: BlockImage, MimeType: "image/png", Data: ""}},
		}}},
	}
	out := TransformMessages(msgs)

	u := out[0].Content
	if u[1].Kind != BlockText || u[1].Text != emptyImagePlaceholder {
		t.Fatalf("user empty image not replaced: %+v", u[1])
	}
	if u[2].Kind != BlockImage || u[2].Data != "aGk=" {
		t.Fatalf("valid user image lost: %+v", u[2])
	}

	tr := out[1].Content[0].Content
	if tr[1].Kind != BlockText || tr[1].Text != emptyImagePlaceholder {
		t.Fatalf("tool-result empty image not replaced: %+v", tr[1])
	}

	// Request-time only: the stored transcript is untouched.
	if msgs[0].Content[1].Kind != BlockImage || msgs[0].Content[1].Data != "" {
		t.Fatal("stored transcript was mutated")
	}
}

// Only the newest keepRecentImageMessages image-bearing messages
// replay their images; older ones downgrade to a text placeholder
// (tool-result images included). Within the quota nothing changes.
func TestTransformMessagesLimitsOldImages(t *testing.T) {
	img := func() Block { return Block{Kind: BlockImage, MimeType: "image/png", Data: "aGk="} }
	var msgs []Message
	msgs = append(msgs, Message{Role: RoleSystem, Content: []Block{TextBlock("sys")}})
	for i := 0; i < keepRecentImageMessages+2; i++ {
		msgs = append(msgs, Message{Role: RoleUser, Content: []Block{img()}})
	}
	// A tool result whose image is nested — the 13th image-bearing
	// message overall and the newest, so it stays inside the quota.
	msgs = append(msgs, Message{Role: RoleTool, Content: []Block{{
		Kind: BlockToolResult, ID: "t1", Content: []Block{TextBlock("shot"), img()},
	}}})

	out := TransformMessages(msgs)

	// 13 image-bearing messages, quota 10: the three oldest (index
	// 1, 2, 3) downgrade.
	for _, i := range []int{1, 2, 3} {
		if out[i].Content[0].Kind != BlockText || out[i].Content[0].Text != oldImagePlaceholder {
			t.Fatalf("message %d not downgraded: %+v", i, out[i].Content[0])
		}
	}
	// Everything from index 4 on keeps its image.
	for i := 4; i < len(out)-1; i++ {
		if out[i].Content[0].Kind != BlockImage {
			t.Fatalf("message %d lost its image inside the quota: %+v", i, out[i].Content[0])
		}
	}
	if tr := out[len(out)-1].Content[0].Content[1]; tr.Kind != BlockImage {
		t.Fatalf("nested tool-result image lost inside the quota: %+v", tr)
	}
	// Stored messages untouched (request-time only).
	if msgs[1].Content[0].Kind != BlockImage {
		t.Fatal("stored transcript was mutated")
	}

	// Within the quota the slice passes through unchanged.
	few := msgs[:5]
	if got := TransformMessages(few); len(got) != len(few) || got[1].Content[0].Kind != BlockImage {
		t.Fatal("within-quota images must pass through")
	}
}
