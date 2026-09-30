package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestVisualDumpOverlay prints the palette and picker panels for manual
// inspection (SCODE_VISUAL_DUMP=1 go test -run TestVisualDumpOverlay -v).
// Not an assertion — a visual check helper.
func TestVisualDumpOverlay(t *testing.T) {
	if os.Getenv("SCODE_VISUAL_DUMP") == "" {
		t.Skip("set SCODE_VISUAL_DUMP=1 to dump overlay panels")
	}
	app := setupImageApp(t)
	m := newModel(app, make(chan any, 16))
	m.width, m.height = 80, 24

	// Palette with a query and one navigation step.
	m.input.SetValue("/re")
	m.updatePalette()
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = tm.(model)
	t.Log("\n" + plainText(m.paletteView()))

	// Picker over a temp directory with mixed entries; pump the Init
	// cmd chain so the listing arrives synchronously.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "shot.png"), []byte("png"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644) //nolint:errcheck
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)                      //nolint:errcheck
	old := m.app.CWD
	m.app.CWD = dir
	for cmd := m.openPicker(); cmd != nil; {
		msg := cmd()
		if msg == nil {
			break
		}
		var next tea.Cmd
		m.picker, next = m.picker.Update(msg)
		cmd = next
	}
	t.Log("\n" + plainText(m.pickerView()))
	m.app.CWD = old
}
