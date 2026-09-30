package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// @ file picker: typing "@" on a fresh token opens a directory browser
// (bubbles filepicker) above the composer. Selecting an image file
// attaches it as a pending [图片#N] attachment — but only when the
// current model accepts image input; otherwise, like every non-media
// file, the selection inserts the path as text for the model to read
// with its tools. A fixed "c" entry attaches the clipboard image.

// pickerRows caps the browser window (plus the header, the whole
// overlay stays under a third of a typical terminal).
const pickerRows = 10

// atTokenStart reports whether the next typed character starts a new
// token (input empty or ending in whitespace) — the condition for "@"
// to open the picker instead of landing in the input.
func (m *model) atTokenStart() bool {
	v := m.input.Value()
	return v == "" || strings.HasSuffix(v, " ") || strings.HasSuffix(v, "\n")
}

// openPicker builds a fresh picker rooted at the session cwd. The
// returned cmd performs the initial directory read.
func (m *model) openPicker() tea.Cmd {
	p := filepicker.New()
	p.CurrentDirectory = m.app.CWD
	p.ShowHidden = false
	p.ShowPermissions = false
	// No size/permission columns: with the "> " cursor at the composer's
	// prompt column, the FILENAME lands exactly on the shared text column.
	p.ShowSize = false
	p.AutoHeight = false
	p.SetHeight(pickerRows)
	p.Styles.Cursor = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	p.Styles.Selected = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
	m.picker = p
	m.pickerOpen = true
	m.resize()
	return p.Init()
}

// closePicker dismisses the overlay and re-flows the layout.
func (m *model) closePicker() {
	m.pickerOpen = false
	m.resize()
}

// attachPickedFile finalizes a picker selection: image files attach
// when the model supports image input; anything else (or an unsupported
// model) inserts the path as text the model can read with its tools.
func (m *model) attachPickedFile(path string) {
	if isImagePath(path) && m.app.Model.Caps.ImageInput {
		if m.attachImageFile(path) {
			return
		}
		m.appendBlock(errStyle.Render(fmt.Sprintf("无法附加 %s（不是受支持的图片或超过大小上限）— 已插入路径", filepath.Base(path))))
	} else if isImagePath(path) {
		m.appendBlock(dimStyle.Render("(当前模型不支持图片输入 — 已插入路径，模型可自行读取文件)"))
	}
	m.input.InsertString(m.relPath(path) + " ")
	m.updatePalette()
	m.resize()
}

// relPath shortens a path for display when it lives under the cwd.
func (m *model) relPath(path string) string {
	if rel, err := filepath.Rel(m.app.CWD, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// isImagePath applies the same extension gate as prompt path scanning.
func isImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

// pickerView renders the browser with a header carrying the key hints
// and the model's image capability.
func (m *model) pickerView() string {
	var b strings.Builder
	hints := "↑↓ 浏览 · enter 进入/选择 · c 剪贴板 · esc 取消"
	if m.app.Model.Caps.ImageInput {
		hints += dimStyle.Render("  · 图片→附件 · 其它→路径")
	} else {
		hints += warnStyle.Render("  · 模型不支持图片，一律插路径")
	}
	b.WriteString(dimStyle.Render("  " + hints))
	b.WriteString("\n")
	b.WriteString(m.picker.View())
	return m.overlayView("选择文件", b.String())
}
