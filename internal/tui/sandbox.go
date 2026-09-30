package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
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

var sandboxModes = []sandboxMode{
	{value: "read-only", desc: "禁止一切文件修改"},
	{value: "workspace-write", desc: "仅工作区与临时目录可写"},
	{value: "danger-full-access", desc: "文件修改不受沙箱限制"},
}

// sandboxBadge maps a mode to its short word + accent color (the status
// bar and the picker share the vocabulary): cyan = read-only, blue =
// workspace-write, orange = unrestricted.
func sandboxBadge(mode string) (string, string) {
	switch mode {
	case "read-only":
		return "只读", "6"
	case "workspace-write":
		return "可写", "39"
	case "danger-full-access":
		return "不限制", "214"
	}
	return "?", "8"
}

// sandboxNote is the transcript echo after a switch (desktop parity).
func sandboxNote(mode string) string {
	switch mode {
	case "danger-full-access":
		return "沙箱已关闭(不限制文件修改)"
	case "workspace-write":
		return "沙箱:仅工作区可写"
	case "read-only":
		return "沙箱:只读(禁止文件修改)"
	}
	return "沙箱:" + mode
}

// openSandbox opens the picker with the highlight on the current mode.
func (m *model) openSandbox() {
	m.sandboxOpen = true
	m.sandboxIdx = 0
	for i, md := range sandboxModes {
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
	if idx < 0 || idx >= len(sandboxModes) {
		return
	}
	mode := sandboxModes[idx].value
	if mode == m.app.SandboxMode() {
		return
	}
	if err := m.app.SetSandboxMode(mode); err != nil {
		m.appendBlock(errStyle.Render("error: " + err.Error()))
		return
	}
	m.appendBlock(noteLine(sandboxNote(mode)))
}

// sandboxView renders the picker overlay: one row per mode (badge word
// colored like the status bar, description dim, the active mode marked),
// the highlight following sandboxIdx, and a key hint.
func (m *model) sandboxView() string {
	cur := m.app.SandboxMode()
	var rows []string
	for i, md := range sandboxModes {
		word, color := sandboxBadge(md.value)
		row := statusFg(color).Render(word) + strings.Repeat(" ", 8-lipgloss.Width(word)) +
			dimStyle.Render(md.desc)
		if md.value == cur {
			row += dimStyle.Render(" · 当前")
		}
		if i == m.sandboxIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · Enter 应用 · Esc 取消"))
	return m.overlayView("沙箱模式", overlayList(rows))
}
