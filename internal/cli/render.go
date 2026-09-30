package cli

import (
	"fmt"
	"io"

	"scode/internal/agent"
	"scode/internal/llm"
)

// Renderer streams agent events to a terminal. Assistant text flows to
// Out (stdout); thinking, tool activity and errors to Err (stderr) so
// print mode stays pipeable.
type Renderer struct {
	Out io.Writer
	Err io.Writer

	inText     bool // inside an assistant text block
	inThinking bool // inside a thinking block
}

func (r *Renderer) Handle(ev agent.Event) {
	switch ev.Type {
	case agent.EvLLM:
		if ev.LLM == nil {
			return
		}
		switch ev.LLM.Type {
		case llm.EventTextDelta:
			fmt.Fprint(r.Out, ev.LLM.Delta)
			r.inText = true
		case llm.EventTextEnd:
			if r.inText {
				fmt.Fprint(r.Out, "\n")
				r.inText = false
			}
		case llm.EventThinkingDelta:
			fmt.Fprint(r.Err, ev.LLM.Delta)
			r.inThinking = true
		case llm.EventThinkingEnd:
			if r.inThinking {
				fmt.Fprint(r.Err, "\n")
				r.inThinking = false
			}
		}
	case agent.EvToolStart:
		if ev.Call != nil {
			fmt.Fprintf(r.Err, "→ %s %s\n", ev.Call.Name, briefArgs(ev.Call.Arguments))
		}
	case agent.EvToolEnd:
		if ev.Result != nil && ev.Result.IsError && ev.Call != nil {
			fmt.Fprintf(r.Err, "✗ %s failed\n", ev.Call.Name)
		}
	case agent.EvAgentError:
		if ev.Err != nil {
			fmt.Fprintf(r.Err, "error: %v\n", ev.Err)
		}
	}
}

// briefArgs renders tool arguments on one line for status output.
func briefArgs(raw []byte) string {
	const max = 72
	s := string(raw)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
