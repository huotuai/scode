package cli

import "sort"

// CommandInfo describes one slash command for the TUI completion
// palette: the canonical name, an args usage hint, and a one-line
// description. App.Command remains the executor; this list is the
// display metadata (keep the two in sync).
type CommandInfo struct {
	Name string // "/reload"
	Hint string // args usage, e.g. "[entryCount]" ("" = no args)
	Desc string
}

// BuiltinCommands lists the slash commands App.Command understands,
// ordered alphabetically for the palette. /skill:name entries come
// from SkillCommands.
func BuiltinCommands() []CommandInfo {
	return []CommandInfo{
		{Name: "/agents", Desc: "configure sub-agents: model + reasoning effort per agent (manager overlay in the TUI, listing in the REPL)"},
		{Name: "/compact", Hint: "[instructions]", Desc: "compact the context (summary + recent tail)"},
		{Name: "/config", Desc: "basic configuration panel: auto-compact, log retention, memory/checkpoint switches"},
		{Name: "/cost", Desc: "usage, cost, context occupancy and per-category breakdown"},
		{Name: "/exit", Desc: "quit (alias: /quit)"},
		{Name: "/fork", Hint: "[entryCount]", Desc: "clone this session"},
		{Name: "/model", Hint: "[name]", Desc: "switch the model (picker in the TUI, name match in the REPL)"},
		{Name: "/models", Desc: "configure a model from the preset catalog (vendor → key → model)"},
		{Name: "/mcp", Desc: "configure/switch MCP servers (manager overlay in the TUI, listing in the REPL)"},
		{Name: "/memory", Hint: "[extract]", Desc: "long-term memory (manager overlay in the TUI; extract runs a pass now)"},
		{Name: "/new", Desc: "start a new session in place (the old one stays resumable)"},
		{Name: "/plan", Hint: "[off]", Desc: "plan mode: read-only research, then approve a plan"},
		{Name: "/reload", Desc: "rescan skills (hot reload, no restart)"},
		{Name: "/rewind", Hint: "[id|last]", Desc: "roll files back to an AI-edit checkpoint (overlay in the TUI)"},
		{Name: "/resume", Hint: "<id>", Desc: "resume needs a restart: scode --resume <id>"},
		{Name: "/sessions", Desc: "list sessions"},
	}
}

// SkillCommands maps the live skill set onto /skill:name palette
// entries (read per render, so a hot reload shows up immediately).
func (a *App) SkillCommands() []CommandInfo {
	list := a.skillList()
	out := make([]CommandInfo, 0, len(list))
	for _, s := range list {
		out = append(out, CommandInfo{
			Name: "/skill:" + s.Name,
			Hint: "[args]",
			Desc: s.Description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
