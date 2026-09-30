package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The /config panel: toggles persist immediately, auto-compact applies
// live (the compaction threshold re-parks), retention cycles presets
// and prunes, the input box hides, esc closes.
func TestConfigPanelFlow(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)

	m.runCommand("/config")
	if !m.cfgOpen {
		t.Fatal("panel did not open")
	}
	if !m.inputHidden() {
		t.Fatal("/config should hide the input box")
	}
	// The shipped defaults: auto-compact on, retention never, the
	// memory/checkpoint switches per the documented defaults, clipboard
	// watching on.
	if !m.cfgSnapshot.AutoCompact || m.cfgSnapshot.LogRetentionDays != 0 ||
		!m.cfgSnapshot.AutoMemory || !m.cfgSnapshot.TypedMemory ||
		m.cfgSnapshot.MemoryRelevance || m.cfgSnapshot.MemoryAutoExtract ||
		!m.cfgSnapshot.RewindCheckpoints || !m.cfgSnapshot.ClipboardWatch {
		t.Fatalf("defaults = %+v", m.cfgSnapshot)
	}
	view := plain(m.configView())
	for _, want := range []string{"上下文自动压缩", "日志清理周期", "Auto Memory", "Typed Memory",
		"Memory Relevance", "Memory Auto Extraction", "Rewind code", "剪贴板图片读取", "永不清理"} {
		if !strings.Contains(view, want) {
			t.Fatalf("panel missing %q:\n%s", want, view)
		}
	}

	// Row 1 (auto-compact) toggles off: persisted.
	m = press(t, m, tea.KeyEnter)
	if m.cfgSnapshot.AutoCompact {
		t.Fatal("toggle did not flip")
	}
	if again := app.TUIConfig(); again.AutoCompact {
		t.Fatal("toggle did not persist")
	}

	// Retention cycles: 永不 → 7 → 14 → 30 → 90 → 永不.
	m = press(t, m, tea.KeyDown) // → retention
	for _, want := range []int{7, 14, 30, 90, 0} {
		m = press(t, m, tea.KeyEnter)
		if m.cfgSnapshot.LogRetentionDays != want {
			t.Fatalf("retention = %d, want %d", m.cfgSnapshot.LogRetentionDays, want)
		}
	}
	if again := app.TUIConfig(); again.LogRetentionDays != 0 {
		t.Fatalf("retention wrap did not persist: %+v", again)
	}

	// Rewind defaults on; toggling persists. Clipboard watching toggles
	// with a live effect on the watcher flag.
	m = press(t, m, tea.KeyDown) // → autoMemory
	m = press(t, m, tea.KeyDown) // → typedMemory
	m = press(t, m, tea.KeyDown) // → relevance
	m = press(t, m, tea.KeyDown) // → autoExtract
	m = press(t, m, tea.KeyDown) // → rewind
	m = press(t, m, 't')
	if m.cfgSnapshot.RewindCheckpoints || app.TUIConfig().RewindCheckpoints {
		t.Fatalf("rewind toggle did not persist: %+v", m.cfgSnapshot)
	}
	m = press(t, m, tea.KeyDown) // → clipboard watch
	m = press(t, m, tea.KeyEnter)
	if m.cfgSnapshot.ClipboardWatch || m.clipWatch {
		t.Fatalf("clipboard watch off not applied live: snapshot=%v flag=%v", m.cfgSnapshot.ClipboardWatch, m.clipWatch)
	}
	if again := app.TUIConfig(); again.ClipboardWatch {
		t.Fatal("clipboard watch toggle did not persist")
	}
	m = press(t, m, tea.KeyEnter) // back on
	if !m.cfgSnapshot.ClipboardWatch || !m.clipWatch {
		t.Fatalf("clipboard watch re-enable failed: %+v flag=%v", m.cfgSnapshot, m.clipWatch)
	}

	m = press(t, m, tea.KeyEscape)
	if m.cfgOpen {
		t.Fatal("esc did not close the panel")
	}
	if m.input.Value() != "" {
		t.Fatalf("input polluted: %q", m.input.Value())
	}
}
