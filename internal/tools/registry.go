package tools

import "scode/internal/agent"

// NewCodingRegistry builds the default tool set. pi's default active
// list is read/bash/edit/write; scode includes grep/find/ls up front —
// search tools pay for themselves in every real session — plus the
// background-task pair that lets a detached bash command stay observable.
// codemap closes the "structure first" gap: outline a package before
// reading files. The web pair registers zero-config defaults here; the
// cli re-adds instances bound to the live settings (Add replaces in
// place, keeping declaration order cache-stable).
func NewCodingRegistry() *agent.Registry {
	return agent.NewRegistry(
		ReadTool{},
		BashTool{},
		BashStatusTool{},
		BashKillTool{},
		EditTool{},
		WriteTool{},
		GrepTool{},
		FindTool{},
		LsTool{},
		CodeMapTool{},
		WebFetchTool{},
		WebSearchTool{},
	)
}
