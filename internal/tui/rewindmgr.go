package tui

// /rewind: the checkpoint browser — the session's AI-edit rewind
// points (newest first). Enter arms a restore, Enter again rolls the
// files back; esc backs out. Restores are host-side writes (the user's
// explicit command), the conversation is untouched.

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	"scode/internal/checkpoint"
)

// tooLargeFiles counts the checkpoint's over-cap (unarchived) files.
func tooLargeFiles(cp checkpoint.Checkpoint) int {
	n := 0
	for _, f := range cp.Files {
		if f.TooLarge {
			n++
		}
	}
	return n
}

func (m *model) openRewindManager() {
	m.rewindOpen = true
	m.rewindConfirm = false
	m.rewindRefresh()
	if m.rewindIdx >= len(m.rewindPoints) {
		m.rewindIdx = 0
	}
	m.resize()
}

func (m *model) rewindRefresh() {
	m.rewindPoints = m.app.Checkpoints()
}

func (m model) handleRewindKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.rewindConfirm {
			m.rewindConfirm = false
			return m, nil
		}
		m.rewindOpen = false
		m.resize()
		return m, nil
	case "up":
		if n := len(m.rewindPoints); n > 0 {
			m.rewindIdx = (m.rewindIdx - 1 + n) % n
		}
		m.rewindConfirm = false
		return m, nil
	case "down":
		if n := len(m.rewindPoints); n > 0 {
			m.rewindIdx = (m.rewindIdx + 1) % n
		}
		m.rewindConfirm = false
		return m, nil
	case "enter", "ctrl+m":
		if m.rewindIdx >= len(m.rewindPoints) {
			return m, nil
		}
		if !m.rewindConfirm {
			m.rewindConfirm = true
			return m, nil
		}
		cp := m.rewindPoints[m.rewindIdx]
		n, err := m.app.CheckpointRestore(cp.ID)
		if err != nil {
			return m, m.showToast("rewind: " + err.Error())
		}
		m.rewindConfirm = false
		m.rewindRefresh()
		return m, m.showToast("已回滚到 " + cp.ID + " · 恢复 " + strconv.Itoa(n) + " 个文件")
	}
	return m, nil // modal: swallow everything else
}

func (m *model) rewindView() string {
	var rows []string
	for i, cp := range m.rewindPoints {
		files := "1 个文件"
		if len(cp.Files) != 1 {
			files = strconv.Itoa(len(cp.Files)) + " 个文件"
		}
		if n := tooLargeFiles(cp); n > 0 {
			files += "," + strconv.Itoa(n) + " 个过大未留档"
		}
		row := cp.ID + " · " + cp.Time.Format("15:04:05") + " · " + cp.Tool + " · " + cp.Summary + dimStyle.Render(" ("+files+")")
		if i == m.rewindIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	if len(rows) == 0 {
		rows = append(rows, dimStyle.Render("  (暂无检查点 — AI 的 edit/write 修改会自动留档)"))
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · enter 两次确认恢复 · esc 返回"))
	if m.rewindConfirm && m.rewindIdx < len(m.rewindPoints) {
		rows = append(rows, warnStyle.Render("  再按 enter 把文件回滚到 "+m.rewindPoints[m.rewindIdx].ID+" (对话不受影响)"))
	}
	return m.overlayView("回滚代码 · 检查点", overlayList(rows))
}
