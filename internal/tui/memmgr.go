package tui

// /memory: the long-term memory manager — the categorized list with
// per-entry curation (s toggles the relevance-selection pick, d
// deletes) and the manual extraction trigger (e, for when Memory Auto
// Extraction is off). Changes persist immediately.

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"scode/internal/memory"
)

func (m *model) openMemoryManager() {
	m.memOpen = true
	m.memConfirmDel = false
	m.memRefresh()
	m.resize()
}

func (m *model) memRefresh() {
	entries, err := m.app.MemoryEntries()
	if err != nil {
		return // keep the previous list; operations surface their own errors
	}
	m.memEntries = entries
}

func (m model) handleMemoryKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.memOpen = false
		m.resize()
		return m, nil
	case "up":
		if n := len(m.memEntries); n > 0 {
			m.memIdx = (m.memIdx - 1 + n) % n
		}
		m.memConfirmDel = false
		return m, nil
	case "down":
		if n := len(m.memEntries); n > 0 {
			m.memIdx = (m.memIdx + 1) % n
		}
		m.memConfirmDel = false
		return m, nil
	case "s":
		if idx := m.memIdx; idx < len(m.memEntries) {
			if _, err := m.app.MemoryToggleSelected(m.memEntries[idx].ID); err != nil {
				return m, m.showToast("memory: " + err.Error())
			}
			m.memRefresh()
		}
		return m, nil
	case "d":
		idx := m.memIdx
		if idx >= len(m.memEntries) {
			return m, nil
		}
		if !m.memConfirmDel {
			m.memConfirmDel = true
			return m, nil
		}
		if err := m.app.MemoryDelete(m.memEntries[idx].ID); err != nil {
			return m, m.showToast("memory: " + err.Error())
		}
		m.memConfirmDel = false
		m.memRefresh()
		if m.memIdx >= len(m.memEntries) && m.memIdx > 0 {
			m.memIdx--
		}
		return m, m.showToast("记忆已删除")
	case "e":
		m.memExtractAsync()
		return m, nil
	}
	return m, nil // modal: swallow everything else
}

// memExtractAsync runs the extraction pass on its own goroutine (the
// model call blocks up to its own timeout); the summary lands as a
// transcript note through the ui channel.
func (m *model) memExtractAsync() {
	m.appendBlock(noteLine("记忆提取中…"))
	app, ui := m.app, m.ui
	go func() {
		summary, err := app.MemoryExtract(context.Background())
		text := summary
		if err != nil {
			text = "memory: " + err.Error()
		}
		ui <- logMsg(text)
	}()
}

func (m *model) memoryView() string {
	var rows []string
	for i, e := range m.memEntries {
		mark := "○"
		if e.Selected {
			mark = "●"
		}
		row := mark + " " + memory.CategoryLabel(e.Category) + " · " + truncate(e.Content, m.width-16)
		if i == m.memIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	if len(rows) == 0 {
		rows = append(rows, dimStyle.Render("  (暂无记忆 — e 立即提取,或在 /config 开启会话结束自动提取)"))
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · s 已选(Relevance 开启时仅注入已选) · d 删除 · e 提取 · esc 关闭"))
	if m.memConfirmDel && m.memIdx < len(m.memEntries) {
		rows = append(rows, warnStyle.Render("  再按 d 确认删除"))
	}
	return m.overlayView("长期记忆", overlayList(rows))
}
