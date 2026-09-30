package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
)

// stubClipboard swaps the clipboard read seam for the test (restored on
// cleanup).
func stubClipboard(t *testing.T, img func() ([]byte, error)) {
	t.Helper()
	oimg := clipboardImageFn
	t.Cleanup(func() { clipboardImageFn = oimg })
	clipboardImageFn = img
}

// setupImageApp is setupPaletteApp with a provider that accepts image
// input (Caps.ImageInput=true).
func setupImageApp(t *testing.T) *cli.App {
	t.Helper()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": "http://127.0.0.1:1", "model": "m", "imageInput": true},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
	app, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() }) //nolint:errcheck
	return app
}

// On-demand reading: ctrl+v probes the clipboard once per press; a
// hit attaches immediately (no loop armed); a miss arms the bounded
// watch loop covered by TestClipboardWatchLoop.
func TestClipboardOnDemand(t *testing.T) {
	app := setupImageApp(t)
	probes := 0
	stubClipboard(t, func() ([]byte, error) {
		probes++
		return tinyPNG(t), nil
	})
	m := newModel(app, make(chan any, 16))
	if !m.clipWatch {
		t.Fatal("on-demand read disabled by default")
	}
	if probes != 0 {
		t.Fatalf("startup probed the clipboard %d times (must be zero)", probes)
	}

	// ctrl+v attaches and echoes the placeholder.
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	m = tm.(model)
	if len(m.clipImgs) != 1 || !strings.Contains(m.input.Value(), "[图片#1]") {
		t.Fatalf("ctrl+v did not attach: clipImgs=%+v input=%q", m.clipImgs, m.input.Value())
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want exactly 1 per press", probes)
	}

	// A second press reads again (each press is independent).
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	m = tm.(model)
	if len(m.clipImgs) != 2 || probes != 2 {
		t.Fatalf("second press: clipImgs=%d probes=%d", len(m.clipImgs), probes)
	}

	// A miss consumes nothing and attaches nothing — falls through.
	stubClipboard(t, func() ([]byte, error) { return nil, errNoClipboardImage })
	if m.attachClipboardImage() {
		t.Fatal("a miss should fall through to text paste")
	}
}

// The watch loop: a ctrl+v miss arms bounded polling; ticks re-probe
// until an image appears (attach + stop), stale ticks from a
// superseded loop are ignored, expiry reports and stops, and the next
// ctrl+v re-arms the loop.
func TestClipboardWatchLoop(t *testing.T) {
	app := setupImageApp(t)
	probes := 0
	hit := false
	stubClipboard(t, func() ([]byte, error) {
		probes++
		if !hit {
			return nil, errNoClipboardImage
		}
		return tinyPNG(t), nil
	})
	m := newModel(app, make(chan any, 16))

	// ctrl+v with no image: one immediate probe, the loop is armed and
	// a notice announces the monitoring window.
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	m = tm.(model)
	if probes != 1 {
		t.Fatalf("immediate probe = %d, want 1", probes)
	}
	if m.clipLoopUntil.IsZero() {
		t.Fatal("a miss did not arm the watch loop")
	}
	if n := len(m.blocks); n == 0 || !strings.Contains(stripANSI(m.blocks[n-1].rendered), "监控中") {
		t.Fatal("arming notice missing")
	}

	// Ticks poll until the image shows up; a miss re-arms the next
	// tick, a hit attaches and stops the loop.
	gen := m.clipLoopGen
	tm, cmd := m.update(clipLoopTickMsg{gen: gen})
	m = tm.(model)
	if probes != 2 || cmd == nil {
		t.Fatalf("miss tick: probes=%d cmd-nil=%v (want re-armed)", probes, cmd == nil)
	}
	hit = true
	tm, cmd = m.update(clipLoopTickMsg{gen: gen})
	m = tm.(model)
	if len(m.clipImgs) != 1 || !strings.Contains(m.input.Value(), "[图片#1]") {
		t.Fatalf("watch hit did not attach: clipImgs=%+v input=%q", m.clipImgs, m.input.Value())
	}
	if cmd != nil || !m.clipLoopUntil.IsZero() {
		t.Fatal("watch did not stop after the hit")
	}

	// The next ctrl+v re-arms the loop; a stale tick from the previous
	// generation is a no-op; expiry reports and stops.
	hit = false
	before := probes
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	m = tm.(model)
	if probes != before+1 || m.clipLoopUntil.IsZero() {
		t.Fatalf("next ctrl+v did not re-arm: probes=%d until=%v", probes, m.clipLoopUntil)
	}
	tm, _ = m.update(clipLoopTickMsg{gen: m.clipLoopGen - 1})
	m = tm.(model)
	if probes != before+1 {
		t.Fatal("stale tick probed the clipboard")
	}
	m.clipLoopUntil = time.Now().Add(-time.Second)
	tm, cmd = m.update(clipLoopTickMsg{gen: m.clipLoopGen})
	m = tm.(model)
	if cmd != nil || !m.clipLoopUntil.IsZero() {
		t.Fatal("expiry did not stop the watch")
	}
	if n := len(m.blocks); n == 0 || !strings.Contains(stripANSI(m.blocks[n-1].rendered), "超时") {
		t.Fatal("expiry notice missing")
	}
}

// clipboardWatch=false: ctrl+v never touches the clipboard API at all
// (the AV-conscious escape hatch).
func TestClipboardGateOff(t *testing.T) {
	app := setupImageApp(t)
	probes := 0
	stubClipboard(t, func() ([]byte, error) {
		probes++
		return tinyPNG(t), nil
	})
	f := false
	app.Settings.ClipboardWatch = &f
	m := newModel(app, make(chan any, 16))
	if m.clipWatch {
		t.Fatal("gate on despite clipboardWatch=false")
	}
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	m = tm.(model)
	if probes != 0 || len(m.clipImgs) != 0 {
		t.Fatalf("gated ctrl+v touched the clipboard: probes=%d clipImgs=%d", probes, len(m.clipImgs))
	}
}

// A paste carrying image file paths becomes attachments immediately and
// drops the path tokens; a text-only paste passes through untouched.
func TestHandlePasteImagePath(t *testing.T) {
	app := setupImageApp(t)
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(pngPath, tinyPNG(t), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newModel(app, make(chan any, 16))

	if !m.handlePaste(`"` + pngPath + `" 这是截图`) {
		t.Fatal("image-path paste not consumed")
	}
	if len(m.clipImgs) != 1 {
		t.Fatalf("pasted path not attached: %+v", m.clipImgs)
	}
	if got := m.input.Value(); strings.Contains(got, pngPath) || !strings.Contains(got, "这是截图") {
		t.Fatalf("remainder wrong: %q", got)
	}

	// Non-image paste passes through (returns false; textarea inserts).
	if m.handlePaste("普通文本 notes.txt") {
		t.Fatal("text paste should not be consumed")
	}
}

// alt+v / ctrl+shift+v are the fallback triggers for terminals that
// swallow ctrl+v (Windows Terminal's own paste action): same
// attach-on-hit / arm-on-miss contract.
func TestClipTriggerKeys(t *testing.T) {
	app := setupImageApp(t)
	hit := true
	stubClipboard(t, func() ([]byte, error) {
		if !hit {
			return nil, errNoClipboardImage
		}
		return tinyPNG(t), nil
	})
	m := newModel(app, make(chan any, 16))

	tm, _ := m.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModAlt})
	m = tm.(model)
	if len(m.clipImgs) != 1 || !strings.Contains(m.input.Value(), "[图片#1]") {
		t.Fatalf("alt+v did not attach: clipImgs=%+v input=%q", m.clipImgs, m.input.Value())
	}

	hit = false
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl | tea.ModShift})
	m = tm.(model)
	if m.clipLoopUntil.IsZero() {
		t.Fatal("ctrl+shift+v miss did not arm the watch loop")
	}
}

// The @ picker: opens on a fresh-token @, closes on esc, and selection
// routes by capability — image attaches (supported model), image falls
// back to path text (unsupported model), non-media inserts the path.
func TestPickerFlow(t *testing.T) {
	app := setupImageApp(t)
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "shot.png")
	txtPath := filepath.Join(dir, "notes.txt")
	os.WriteFile(pngPath, tinyPNG(t), 0o644)   //nolint:errcheck
	os.WriteFile(txtPath, []byte("hi"), 0o644) //nolint:errcheck

	m := newModel(app, make(chan any, 16))
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: '@', Text: "@"})
	m = tm.(model)
	if !m.pickerOpen {
		t.Fatal("@ did not open the picker")
	}
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(model)
	if m.pickerOpen {
		t.Fatal("esc did not close the picker")
	}

	// Supported model: image selection attaches.
	m.attachPickedFile(pngPath)
	if len(m.clipImgs) != 1 || !strings.Contains(m.input.Value(), "[图片#1]") {
		t.Fatalf("image not attached: clipImgs=%+v input=%q", m.clipImgs, m.input.Value())
	}
	// Non-media: path text, no attachment.
	m.attachPickedFile(txtPath)
	if len(m.clipImgs) != 1 || !strings.Contains(m.input.Value(), "notes.txt") {
		t.Fatalf("path not inserted: input=%q", m.input.Value())
	}

	// Unsupported model: image falls back to path text.
	app2 := setupPaletteApp(t) // openai-compat without imageInput
	m2 := newModel(app2, make(chan any, 16))
	m2.attachPickedFile(pngPath)
	if len(m2.clipImgs) != 0 || !strings.Contains(m2.input.Value(), "shot.png") {
		t.Fatalf("unsupported model should insert path: input=%q", m2.input.Value())
	}
}

// End-to-end: a bracketed paste carrying an image file path becomes an
// attachment on the provider request (the Windows Terminal
// copy-image-file / drag-drop flow).
func TestProgramPasteImagePath(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "dropped.png")
	if err := os.WriteFile(pngPath, tinyPNG(t), 0o644); err != nil {
		t.Fatal(err)
	}

	reqBodies := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqBodies <- string(b)
		chatSSEBlob(w, "ok")
	}))
	defer srv.Close()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settingsJSON := `{"defaultProvider":"openai-compat","sandbox":{"mode":"danger-full-access"},"providers":{"openai-compat":{"apiKey":"k","baseUrl":"` + srv.URL + `","model":"m","imageInput":true}}}`
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settingsJSON), 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	ui := make(chan any, 256)
	app, err := cli.Setup(cli.Options{Approver: newApprover(ui)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	pr, pw := io.Pipe()
	out := &syncWriter{}
	p := tea.NewProgram(newModel(app, ui),
		tea.WithInput(pr),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 30),
		tea.WithoutSignals(),
	)
	runDone := make(chan error, 1)
	go func() { _, err := p.Run(); runDone <- err }()

	// Bracketed paste of a quoted image path, then the prompt text.
	if err := (scriptWriter{pw}).write("\x1b[200~\"" + pngPath + "\"\x1b[201~"); err != nil {
		t.Fatal(err)
	}
	if err := (scriptWriter{pw}).write("看这张图\r"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var body string
	for body == "" {
		if time.Now().After(deadline) {
			t.Fatalf("provider never called; output:\n%s", stripANSI(out.String()))
		}
		select {
		case body = <-reqBodies:
		case err := <-runDone:
			t.Fatalf("program exited early: %v; output:\n%s", err, stripANSI(out.String()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, want := range []string{`"image_url"`, "看这张图"} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %q; body:\n%s", want, body)
		}
	}
	if strings.Contains(body, pngPath) {
		t.Errorf("path leaked into the request text; body:\n%s", body)
	}
	time.Sleep(200 * time.Millisecond)
	quitProgram(t, pw, runDone)
}
