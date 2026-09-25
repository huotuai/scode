package tools

import "scode/internal/agent"

// NewCodingRegistry builds the default tool set. pi's default active
// list is read/bash/edit/write; scode includes grep/find/ls up front —
// search tools pay for themselves in every real session.
func NewCodingRegistry() *agent.Registry {
	return agent.NewRegistry(
		ReadTool{},
		BashTool{},
		EditTool{},
		WriteTool{},
		GrepTool{},
		FindTool{},
		LsTool{},
	)
}
