package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Esc dismissal must stick: update()'s fall-through feeds every stray
// non-key message (cursor blink, focus events) to the input and
// re-runs updatePalette() from the raw text — without the paletteEsc
// memory the list would pop back open half a second after Esc and
// steal ↑/↓/Enter back from the input. Editing the text re-arms it.
func TestPaletteEscDismissalSticks(t *testing.T) {
	app := setupPaletteApp(t)
	m := newModel(app, make(chan any, 16))
	m = typeKeys(t, m, "/re")
	if !m.paletteOpen {
		t.Fatal("palette did not open on /re")
	}
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(model)
	if m.paletteOpen {
		t.Fatal("esc did not dismiss the palette")
	}
	// Stray non-key messages (blink/focus) must not reopen it.
	for range 3 {
		tm, _ = m.update(tea.FocusMsg{})
		m = tm.(model)
		if m.paletteOpen {
			t.Fatal("palette reopened on a stray message after esc dismissal")
		}
	}
	// Editing the text re-arms the palette.
	m = typeKeys(t, m, "l") // "/rel"
	if !m.paletteOpen {
		t.Fatal("palette did not reopen after editing the dismissed text")
	}
	if len(m.paletteHits) != 1 || m.paletteHits[0].Name != "/reload" {
		t.Fatalf("hits after re-arm = %+v", m.paletteHits)
	}
	// Backspacing back to the dismissed text closes it again.
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = tm.(model)
	if m.paletteOpen {
		t.Fatal("returning to the esc-dismissed text should keep the palette closed")
	}
	// Leaving the command form clears the dismissal memory.
	m = typeKeys(t, m, " ")
	if m.paletteEsc != "" {
		t.Fatal("paletteEsc should clear once the input leaves command form")
	}
}
