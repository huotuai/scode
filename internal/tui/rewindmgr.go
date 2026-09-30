package tui

// /rewind: the checkpoint browser — the session's AI-edit rewind
// points (newest first). Enter arms a restore, Enter again rolls the
// files back; esc backs out. Restores are host-side writes (the user's
// explicit command), the conversation is untouched.

import (
	tea "charm.land/bubbletea/v2"

	"scode/internal/checkpoint"
	"scode/internal/i18n"
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
		return m, m.showToast(i18n.Tf("tui.rewind.restored", cp.ID, n))
	}
	return m, nil // modal: swallow everything else
}

func (m *model) rewindView() string {
	var rows []string
	for i, cp := range m.rewindPoints {
		files := i18n.T("tui.rewind.oneFile")
		if len(cp.Files) != 1 {
			files = i18n.Tf("tui.rewind.nFiles", len(cp.Files))
		}
		if n := tooLargeFiles(cp); n > 0 {
			files += i18n.Tf("tui.rewind.tooLarge", n)
		}
		row := cp.ID + " · " + cp.Time.Format("15:04:05") + " · " + cp.Tool + " · " + cp.Summary + dimStyle.Render(" ("+files+")")
		if i == m.rewindIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	if len(rows) == 0 {
		rows = append(rows, dimStyle.Render(i18n.T("tui.rewind.empty")))
	}
	rows = append(rows, dimStyle.Render(i18n.T("tui.rewind.hint")))
	if m.rewindConfirm && m.rewindIdx < len(m.rewindPoints) {
		rows = append(rows, warnStyle.Render(i18n.Tf("tui.rewind.confirm", m.rewindPoints[m.rewindIdx].ID)))
	}
	return m.overlayView(i18n.T("tui.rewind.title"), overlayList(rows))
}
