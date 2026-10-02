package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"scode/internal/i18n"
)

// /sandbox: an overlay picker for the session's sandbox mode — the TUI
// counterpart of the desktop's SandboxPicker. Selection applies
// immediately: App.SetSandboxMode persists the switch and lands it as a
// transcript section delta.

// sandboxMode is one selectable policy (dsh sandbox vocabulary).
type sandboxMode struct {
	value string
	desc  string
}

// sandboxModes are the selectable policies (dsh sandbox vocabulary).
// A function (not a var) so the display language is read at render time.
func sandboxModes() []sandboxMode {
	return []sandboxMode{
		{value: "read-only", desc: i18n.T("tui.sandbox.readOnlyDesc")},
		{value: "workspace-write", desc: i18n.T("tui.sandbox.workspaceWriteDesc")},
		{value: "danger-full-access", desc: i18n.T("tui.sandbox.fullAccessDesc")},
	}
}

// sandboxBadge maps a mode to its short word + accent color (the status
// bar and the picker share the vocabulary): cyan = read-only, blue =
// workspace-write, orange = unrestricted.
func sandboxBadge(mode string) (string, string) {
	switch mode {
	case "read-only":
		return i18n.T("tui.sandbox.badgeReadOnly"), "6"
	case "workspace-write":
		return i18n.T("tui.sandbox.badgeWritable"), "39"
	case "danger-full-access":
		return i18n.T("tui.sandbox.badgeUnrestricted"), "214"
	}
	return "?", "8"
}

// sandboxNote is the transcript echo after a switch (desktop parity).
func sandboxNote(mode string) string {
	switch mode {
	case "danger-full-access":
		return i18n.T("tui.sandbox.noteOff")
	case "workspace-write":
		return i18n.T("tui.sandbox.noteWorkspace")
	case "read-only":
		return i18n.T("tui.sandbox.noteReadOnly")
	}
	return i18n.Tf("tui.sandbox.noteMode", mode)
}

// openSandbox opens the picker with the highlight on the current mode.
func (m *model) openSandbox() {
	m.sandboxOpen = true
	m.sandboxIdx = 0
	for i, md := range sandboxModes() {
		if md.value == m.app.SandboxMode() {
			m.sandboxIdx = i
		}
	}
	m.resize()
}

// applySandbox switches the mode and echoes the confirmation. Re-picking
// the current mode just closes the picker.
func (m *model) applySandbox(idx int) {
	m.sandboxOpen = false
	m.resize()
	if idx < 0 || idx >= len(sandboxModes()) {
		return
	}
	mode := sandboxModes()[idx].value
	if mode == m.app.SandboxMode() {
		return
	}
	if err := m.app.SetSandboxMode(mode); err != nil {
		m.appendBlock(errStyle.Render("error: " + err.Error()))
		return
	}
	m.appendBlock(noteLine(sandboxNote(mode)))
}

// cycleSandbox is the shift+tab shortcut: step the session's sandbox
// mode one notch (read-only → workspace-write → danger-full-access →
// read-only) without opening the picker. The switch applies
// immediately (the per-call policy is re-resolved for every tool call)
// and persists to the session log + project settings inside
// SetSandboxMode.
func (m *model) cycleSandbox() {
	modes := sandboxModes()
	cur := 0
	for i, md := range modes {
		if md.value == m.app.SandboxMode() {
			cur = i
		}
	}
	next := modes[(cur+1)%len(modes)].value
	if err := m.app.SetSandboxMode(next); err != nil {
		m.appendBlock(errStyle.Render("error: " + err.Error()))
		return
	}
	m.appendBlock(noteLine(sandboxNote(next)))
}

// sandboxView renders the picker overlay: one row per mode (badge word
// colored like the status bar, description dim, the active mode marked),
// the highlight following sandboxIdx, and a key hint.
func (m *model) sandboxView() string {
	cur := m.app.SandboxMode()
	var rows []string
	for i, md := range sandboxModes() {
		word, color := sandboxBadge(md.value)
		// Pad AFTER the badge word, clamped: the English words run wider
		// than the 8-cell budget the Chinese layout assumed (a negative
		// repeat would panic).
		pad := 8 - lipgloss.Width(word)
		if pad < 1 {
			pad = 1
		}
		row := statusFg(color).Render(word) + strings.Repeat(" ", pad) +
			dimStyle.Render(md.desc)
		if md.value == cur {
			row += dimStyle.Render(i18n.T("tui.sandbox.currentMark"))
		}
		if i == m.sandboxIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render(i18n.T("tui.sandbox.hint")))
	return m.overlayView(i18n.T("tui.sandbox.title"), overlayList(rows))
}
