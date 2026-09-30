package tui

import (
	"strings"
	"testing"
)

// Regression: with the palette (or any overlay) above the composer,
// the hardware cursor must anchor at the composer's text cell — the
// "\n" that joins view parts is a line terminator, not an extra row,
// so each pre-input part contributes exactly its rendered height.
func TestPaletteCursorAnchorsInput(t *testing.T) {
	app := setupPaletteApp(t)
	m := newModel(app, make(chan any, 16))
	m.width, m.height = 80, 24
	m.resize()
	m = typeKeys(t, m, "/as")
	if !m.paletteOpen {
		t.Fatal("palette did not open for /as")
	}

	v := m.view()
	if v.Cursor == nil {
		t.Fatal("no hardware cursor")
	}
	lines := strings.Split(v.Content, "\n")
	textRow := -1
	for i, l := range lines {
		if s := stripANSI(l); strings.Contains(s, "/as") && !strings.Contains(s, "─") {
			textRow = i
		}
	}
	if textRow < 0 {
		t.Fatal("composer text row not found in frame")
	}
	if v.Cursor.Position.Y != textRow {
		t.Fatalf("cursor Y=%d, composer text row=%d", v.Cursor.Position.Y, textRow)
	}
	// X: "> /as" — the cursor sits after the text, one cell in for the
	// box's left border: prompt (2) + "/as" (3) + border (1).
	if want := 2 + len("/as") + 1; v.Cursor.Position.X != want {
		t.Fatalf("cursor X=%d, want %d", v.Cursor.Position.X, want)
	}
}
