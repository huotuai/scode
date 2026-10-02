package tui

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"scode/internal/agent"
	"scode/internal/checkpoint"
	"scode/internal/cli"
	"scode/internal/config"
	"scode/internal/i18n"
	"scode/internal/llm"
	"scode/internal/memory"
	"scode/internal/plantrack"
	"scode/internal/subagent"
)

// UI event-stream messages (the single ui channel multiplexes agent
// events, run completions, approval requests, and captured stderr).
type agentEventMsg struct{ ev agent.Event }
type runDoneMsg struct{ err error }
type approvalMsg struct{ req *approvalRequest }
type logMsg string

// flushTickMsg is the streaming frame clock: LLM deltas accumulate
// into the live buffer and only reach the viewport once per
// flushInterval, instead of re-laying-out the transcript per delta.
type flushTickMsg struct{}

// toastExpireMsg clears the toast whose showToast scheduled it; the id
// ensures only the NEWEST toast is cleared (a fresh toast replaces an
// older one, but the older timer still fires).
type toastExpireMsg struct{ id int }

// quitDisarmMsg expires the double-ctrl+c quit guard; the id ensures a
// stale timer never disarms a freshly armed guard.
type quitDisarmMsg struct{ id int }

// quitArmTTL is the window in which a second ctrl+c quits after the
// first one armed the guard.
const quitArmTTL = 2 * time.Second

// flushInterval caps viewport re-syncs during streaming (~30fps).
const flushInterval = 33 * time.Millisecond

// toastTTL is how long a toast floats at the top-right corner.
const toastTTL = 3 * time.Second

// block is one rendered transcript entry. Thinking blocks keep their
// raw source so ctrl+o (expand) and width changes can re-render them;
// assistant text blocks keep theirs so pipe tables re-align on width
// changes instead of hard-cutting (table.go). lines caches the block
// hard-wrapped at the content width, so the scroller never re-lays-out
// old transcript.
type block struct {
	rendered string
	think    string // raw thinking source (isThink only)
	text     string // raw assistant text (isText only)
	isThink  bool
	isText   bool
	spacer   bool     // blank separator line between items (spacing leads items)
	lines    []string // wrapped at contentWidth (append-time cache)
}

// model is the bubbletea root model: a transcript viewport over a
// multi-line input over a status bar, with an approval box rendered
// above the input while a decision is pending.
type model struct {
	app *cli.App
	ui  chan any

	vp            scroller
	input         textarea.Model
	width, height int

	blocks []block // rendered transcript blocks
	// syncedBlocks is the write watermark: blocks [0, syncedBlocks)
	// already had their wrapped lines pushed into the scroller, so a
	// sync moves only new content — O(new), not O(transcript).
	syncedBlocks int
	// blockOff[i] is the scroller line index where block i's lines
	// start, so a settled tool call can re-render its row in place
	// (status dot) without rebuilding the transcript.
	blockOff []int
	// toolCalls maps an open tool call's id to its block index; the
	// dot starts gray (running) and settles green/red/orange on end.
	toolCalls map[string]int
	live      *streamBuf // streaming text/thinking buffer (pointer: model
	// copies share it, and a bare strings.Builder panics on copy)
	dirty bool // live changed since the last viewport sync
	// flushScheduled guards against tick storms: at most one
	// flushInterval tick is in flight.
	flushScheduled bool
	expandThinking bool // ctrl+o: show thinking blocks in full
	contentWidth   int  // width the block line caches were wrapped at
	// Work-status tail line: phase drives the label (Requesting... /
	// Working...), spinFrame steps the star animation.
	phase     workPhase
	spinFrame int

	running bool
	cancel  context.CancelFunc
	queued  []string // slash commands queued mid-run (REPL parity)
	quit    bool     // a queued /exit fired

	// Double-ctrl+c quit guard: the first press arms the guard (and
	// floats a hint toast), only a second press inside quitArmTTL quits,
	// so a stray ctrl+c cannot kill the session. Any other key disarms.
	// quitSeq ids each arming so a stale disarm tick clears nothing.
	quitArmed bool
	quitSeq   int

	pending *approvalRequest

	// Plan review state (render.go planApprovalView): planScroll is the
	// body window's line offset, planBtn the focused action button.
	planScroll int
	planBtn    int
	// approvalBtn is the focused button of a pending tool/sandbox ask
	// (approval.go approvalButtons; plan reviews use planBtn).
	approvalBtn int

	// Pending clipboard image attachments (clipboard_image.go): each is
	// echoed into the input as a [图片#N] placeholder and rides the next
	// submitted prompt; deleting the placeholder drops the attachment.
	clipImgs []clipImage
	imgSeq   int

	// Command completion palette (palette.go): opens when the input is
	// a bare "/..." fragment, narrows greedily per keystroke.
	// paletteEsc remembers the text at the last Esc dismissal: the
	// blink/focus fall-through in update() re-runs updatePalette() from
	// the raw text, which would otherwise reopen the list half a second
	// after Esc. Editing the text re-arms the palette.
	paletteOpen bool
	paletteHits []cli.CommandInfo
	paletteIdx  int
	paletteEsc  string

	// File picker (picker.go): typing "@" on a fresh token opens the
	// overlay; images attach per model capability, anything else inserts
	// its path.
	pickerOpen bool
	picker     filepicker.Model

	// Sandbox mode picker (sandbox.go): /sandbox opens the overlay;
	// sandboxIdx is the highlighted row.
	sandboxOpen bool
	sandboxIdx  int

	// Model picker (modelpicker.go): /model opens a TWO-stage overlay —
	// choose the configured pair, then the reasoning effort; Enter
	// commits both together. modelIdx/modelEffortIdx are the per-stage
	// highlights, modelPending the pair chosen at stage one.
	modelOpen      bool
	modelStage     modelStage
	modelIdx       int
	modelEffortIdx int
	modelPending   int
	modelChoices   []config.ModelChoice

	// Model manager (modelmgr.go): /models opens a FOUR-stage overlay
	// over the preset catalog — vendor → API key → the vendor's models →
	// confirm (summary + reasoning strength). Enter on the last stage
	// writes the profile to settings and switches onto it. keyBuf is the
	// typed key, keyExisting the saved one Enter reuses.
	modelsOpen  bool
	modelsStage modelsStage
	modelsCat   *config.PresetCatalog
	vendorIdx   int
	modelsIdx   int
	confirmIdx  int
	keyBuf      string
	keyExisting string

	// /mcp overlay (mcpmgr.go): runtime MCP configuration, effective
	// immediately. mcpServers is the snapshot refreshed after every
	// mutation.
	mcpOpen       bool
	mcpStage      mcpStage
	mcpServers    []cli.MCPServerInfo
	mcpListIdx    int
	mcpEditField  int
	mcpForm       mcpForm
	mcpConfirmDel bool

	// /agents overlay (agentmgr.go): sub-agent definitions; the task
	// tool resolves them per call, so saves need no registry churn.
	// agentsSpecs is the merged surface (built-ins + overrides);
	// agentUserNames tracks which names have an agents.json entry.
	agentsOpen       bool
	agentsStage      agentsStage
	agentsSpecs      []subagent.Spec
	agentUserNames   map[string]bool
	agentModels      []config.ModelChoice // the form's model picker source (activated models)
	agentsIdx        int
	agentsEditField  int
	agentForm        agentForm
	agentsConfirmDel bool

	// /config panel (cfgmgr.go): the basic-configuration checklist;
	// cfgSnapshot refreshes after every toggle.
	cfgOpen     bool
	cfgIdx      int
	cfgSnapshot cli.TUIConfig

	// /memory overlay (memmgr.go): the long-term memory manager.
	memOpen       bool
	memIdx        int
	memConfirmDel bool
	memEntries    []memory.Entry

	// /rewind overlay (rewindmgr.go): the AI-edit checkpoint browser.
	rewindOpen    bool
	rewindIdx     int
	rewindConfirm bool
	rewindPoints  []checkpoint.Checkpoint

	// clipWatch gates the clipboard-image read (clipboard_image.go):
	// one probe per trigger (alt+v / ctrl+v / ctrl+shift+v) plus a
	// bounded watch loop while a miss is being answered; zero
	// clipboard calls at idle.
	clipWatch bool
	// clipLoop is the armed state of the post-ctrl+v watch: polling
	// stops at clipLoopUntil, and clipLoopGen invalidates ticks from a
	// superseded or stopped loop.
	clipLoopUntil time.Time
	clipLoopGen   int

	// Mouse text selection: left-drag over the transcript selects,
	// right click copies (CellMotion mouse capture suppresses the
	// terminal's native selection — shift+drag still bypasses it).
	// Anchored to absolute line indices so the highlight survives
	// scrolling.
	sel selection

	// Transient top-right toast (copy confirmations and failures);
	// toastSeq ids each toast so only the newest is cleared by its
	// expiry tick.
	toast    *toastInfo
	toastSeq int

	usage *cli.UsageReport // cached status-bar snapshot (the report walks the whole transcript; refreshed off the render path)
}

func newModel(app *cli.App, ui chan any) model {
	ta := textarea.New()
	ta.Prompt = composerPrompt
	// The prompt glyph renders as an accent badge (the style is per
	// focus state; the composer never blurs, but set both).
	styles := ta.Styles()
	styles.Focused.Prompt = composerPromptStyle
	styles.Blurred.Prompt = composerPromptStyle
	ta.SetStyles(styles)
	ta.Placeholder = i18n.T("tui.main.placeholder")
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 8
	ta.CharLimit = 0
	// Enter sends; newline moves to ctrl+j / shift+enter.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "shift+enter"))
	ta.SetWidth(78) // 80 minus the input box's left/right border
	ta.Focus()      // must happen HERE: Init's value receiver would discard it
	// The REAL terminal cursor tracks the composer (view() places it via
	// View.Cursor): IME candidate windows anchor to the hardware cursor,
	// and the textarea's virtual cursor leaves it stranded in the status
	// bar.
	ta.SetVirtualCursor(false)

	vp := scroller{height: 20, pinned: true}

	m := model{app: app, ui: ui, vp: vp, input: ta, width: 80, height: 24, contentWidth: 80}
	m.clipWatch = app.Settings == nil || app.Settings.ClipboardWatch == nil || *app.Settings.ClipboardWatch
	prov, modelID := app.CurrentModel()
	m.appendBlock(welcomeBanner(prov, modelID, app.CWD, app.Sess.Header().ID))
	m.addSpacer()
	// A session that already carries a plan (resume/fork, or a plan set
	// in an earlier run) opens with the checklist in view — the progress
	// state must not depend on watching the updates stream by.
	if plan, ok := app.PlanState(); ok {
		m.appendBlock(planBlock(plan, m.width))
		m.addSpacer()
	}
	m.refreshUsage()
	m.syncViewport()
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, awaitUI(m.ui))
}

// spinTickMsg advances the work-status animation (every spinInterval
// while a run is in flight).
type spinTickMsg struct{}

// spinInterval steps the tail line's animation: the 8-frame cycle takes
// ~1s — half a rotation turn, half a breathing pulse.
const spinInterval = 120 * time.Millisecond

// spinTick arms the animation; nil when idle.
func (m model) spinTick() tea.Cmd {
	if m.phase == phaseIdle {
		return nil
	}
	return tea.Tick(spinInterval, func(time.Time) tea.Msg { return spinTickMsg{} })
}

// awaitUI blocks on the shared event stream; exactly one instance is
// in flight at any time (re-issued by every consumer).
func awaitUI(ch <-chan any) tea.Cmd {
	return func() tea.Msg {
		if msg, ok := <-ch; ok {
			return msg
		}
		return nil
	}
}

func (m model) Update(msg tea.Msg) (tm tea.Model, cmd tea.Cmd) {
	// Log panics before bubbletea's catchPanics swallows the stack into
	// the stderr pipe (recoverFromPanic prints to os.Stderr, which the
	// TUI redirected into the dead event stream).
	defer func() {
		if r := recover(); r != nil {
			writeCrashLog(m.cfgDir(), fmt.Sprintf("update panic: %v\n%s", r, debug.Stack()))
			panic(r)
		}
	}()
	return m.update(msg)
}

func (m model) View() (v tea.View) {
	defer func() {
		if r := recover(); r != nil {
			writeCrashLog(m.cfgDir(), fmt.Sprintf("view panic: %v\n%s", r, debug.Stack()))
			panic(r)
		}
	}()
	return m.view()
}

func (m model) cfgDir() string {
	if m.app != nil {
		return m.app.CfgDir
	}
	return ""
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case agentEventMsg:
		m.renderEvent(msg.ev)
		return m, tea.Batch(awaitUI(m.ui), m.maybeFlushTick())
	case runDoneMsg:
		m.running = false
		m.cancel = nil
		m.phase = phaseIdle // the tail line leaves with the run
		m.flushLive()
		if msg.err != nil {
			m.appendBlock(errStyle.Render("error: " + msg.err.Error()))
		}
		m.refreshUsage()
		queued := m.queued
		m.queued = nil
		for i, c := range queued {
			if cli.IsPromptCommand(c) {
				// A queued prompt command starts a turn of its own; the
				// rest of the queue waits for that turn (one run at a
				// time), so re-queue everything behind it.
				m.queued = append([]string{}, queued[i+1:]...)
				m.ensureSpacer()
				m.addSpacer()
				m.appendBlock(userStyle.Render("✨ " + c))
				m.startRun(c)
				break
			}
			m.runCommand(c)
		}
		if m.quit {
			return m, tea.Quit
		}
		if m.running {
			return m, tea.Batch(awaitUI(m.ui), m.spinTick())
		}
		return m, awaitUI(m.ui)
	case approvalMsg:
		m.pending = msg.req
		m.planScroll, m.planBtn = 0, 0
		m.approvalBtn = 0
		m.resize()
		return m, awaitUI(m.ui)
	case logMsg:
		m.appendBlock(dimStyle.Render(string(msg)))
		return m, awaitUI(m.ui)
	case flushTickMsg:
		m.flushScheduled = false
		if m.dirty {
			m.syncViewport()
		}
		return m, nil
	case spinTickMsg:
		if m.phase != phaseIdle {
			m.spinFrame = (m.spinFrame + 1) % len(spinFrames)
			m.syncTail()
		}
		return m, m.spinTick()
	case clipLoopTickMsg:
		return m, m.onClipLoopTick(msg)
	case tea.PasteMsg:
		// The /models key stage takes pasted text into the key buffer
		// (an API key paste is the common case there).
		if m.modelsOpen && m.modelsStage == modelsStageKey {
			m.modelsKeyType(msg.Content)
			return m, nil
		}
		// The /mcp edit screen pastes into the focused text field.
		if m.mcpOpen && m.mcpStage == mcpStageEdit {
			m.mcpType(msg.Content)
			return m, nil
		}
		// The /agents edit screen pastes into its focused text field.
		if m.agentsOpen && m.agentsStage == agentsStageEdit {
			m.agentsType(msg.Content)
			return m, nil
		}
		// A paste carrying image file paths (Explorer copy, drag-drop,
		// IDE temp paths — terminals paste those as quoted text) becomes
		// attachments on the spot; everything else pastes normally.
		if m.handlePaste(msg.Content) {
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.updatePalette()
		m.resize()
		return m, cmd
	case tea.MouseWheelMsg:
		// While a plan review is pending, the wheel scrolls the plan
		// body, not the transcript behind it.
		if m.pending != nil && m.pending.kind == "plan" {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.scrollPlan(-3)
			case tea.MouseWheelDown:
				m.scrollPlan(3)
			}
			return m, nil
		}
		// Wheel scrolls the transcript, not the input's internal view.
		switch msg.Button {
		case tea.MouseWheelUp:
			m.vp.scrollUp(3) // viewport default wheel delta
		case tea.MouseWheelDown:
			m.vp.scrollDown(3)
		}
		return m, nil
	case tea.MouseClickMsg:
		switch msg.Button {
		case tea.MouseLeft:
			// The viewport owns screen rows [0, vp.Height()); everything
			// below is the approval box / overlays / input.
			if msg.Y < m.vp.Height() {
				m.beginSelection(msg.X, msg.Y)
			}
		case tea.MouseRight:
			return m, m.copySelection()
		}
		return m, nil
	case tea.MouseMotionMsg:
		if msg.Button == tea.MouseLeft && m.sel.active {
			m.extendSelection(msg.X, msg.Y)
		}
		return m, nil
	case tea.MouseReleaseMsg:
		// A plain click (no drag) selects nothing — release clears it,
		// matching native selection semantics.
		if m.sel.active && m.sel.zeroWidth() {
			m.sel = selection{}
		}
		return m, nil
	case toastExpireMsg:
		if m.toast != nil && m.toast.id == msg.id {
			m.toast = nil
		}
		return m, nil
	case quitDisarmMsg:
		if m.quitSeq == msg.id {
			m.quitArmed = false
		}
		return m, nil
	}
	// Everything else (focus, cursor blink, the picker's async dir reads)
	// goes to the open picker and the input.
	var cmd, pcmd tea.Cmd
	if m.pickerOpen {
		m.picker, pcmd = m.picker.Update(msg)
	}
	m.input, cmd = m.input.Update(msg)
	m.updatePalette()
	m.resize()
	return m, tea.Batch(cmd, pcmd)
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	// Any key other than ctrl+c disarms the double-ctrl+c quit guard.
	if k != "ctrl+c" {
		m.quitArmed = false
	}
	// A pending approval owns the keyboard.
	if m.pending != nil {
		req := m.pending
		if k == "esc" {
			req.answer <- "n"
			m.appendBlock(dimStyle.Render("(" + req.kind + ": denied)"))
			m.pending = nil
			m.resize()
			return m, nil
		}
		// Plan review: the body scrolls (↑/↓/pgup/pgdn), the action
		// buttons switch with ←/→/Tab, Enter activates the focused one.
		// Letters never answer — y-typing confirmed too easily by
		// accident — they just draft revision feedback into the input.
		if req.kind == "plan" {
			switch k {
			case "up", "pgup":
				m.scrollPlan(-m.planPage(k))
				return m, nil
			case "down", "pgdn", "pgdown":
				m.scrollPlan(m.planPage(k))
				return m, nil
			case "left", "shift+tab":
				m.planBtn = (m.planBtn - 1 + len(planButtons())) % len(planButtons())
				return m, nil
			case "right", "tab":
				m.planBtn = (m.planBtn + 1) % len(planButtons())
				return m, nil
			case "enter", "ctrl+m":
				if strings.TrimSpace(m.input.Value()) == "" {
					m.answerPlanButton()
					return m, nil
				}
				// typed text = revision feedback; submit() sends it
			}
			// everything else types into the input (feedback draft)
		} else {
			// Tool/sandbox asks are button-driven: the arrow keys move the
			// focus, Enter activates the focused button; the letter keys
			// stay as accelerators.
			if btns := approvalButtons(req.kind); len(btns) > 0 {
				switch k {
				case "left", "up", "shift+tab":
					m.approvalBtn = (m.approvalBtn - 1 + len(btns)) % len(btns)
					return m, nil
				case "right", "down", "tab":
					m.approvalBtn = (m.approvalBtn + 1) % len(btns)
					return m, nil
				case "enter", "ctrl+m":
					idx := min(max(m.approvalBtn, 0), len(btns)-1)
					m.answerApproval(btns[idx].Key)
					return m, nil
				}
			}
			// ctrl+c still aborts the pending run — the same contract as the
			// global handler and every modal. Swallowing it here would trap
			// the user inside the prompt. The ask unblocks as an interruption
			// with the cancellation, so dismiss the prompt too.
			if k == "ctrl+c" {
				if m.running {
					m.cancelRun()
					req.answer <- ""
					m.pending = nil
					m.resize()
					return m, nil
				}
				return m.ctrlCQuit()
			}
			m.answerKey(k) // letter accelerators (ignored when not an option)
			return m, nil
		}
	}
	// The file picker owns the keyboard while open: navigation goes to
	// the picker, "c" attaches the clipboard image, Esc closes (the
	// picker's own Back binding also binds esc — closed here first).
	if m.pickerOpen {
		switch k {
		case "esc":
			m.closePicker()
			return m, nil
		case "c":
			m.closePicker()
			m.attachClipboardImage()
			return m, nil
		case "ctrl+c":
			return m.ctrlCQuit()
		}
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		if sel, path := m.picker.DidSelectFile(msg); sel {
			m.closePicker()
			m.attachPickedFile(path)
			return m, nil
		}
		if disabled, path := m.picker.DidSelectDisabledFile(msg); disabled {
			m.appendBlock(errStyle.Render(i18n.Tf("tui.main.notSelectable", path)))
		}
		return m, cmd
	}
	// The /sandbox picker owns the keyboard while open: ↑/↓ move the
	// highlight, Enter applies, Esc cancels (ctrl+c still quits).
	if m.sandboxOpen {
		switch k {
		case "ctrl+c":
			return m.ctrlCQuit()
		case "esc":
			m.sandboxOpen = false
			m.resize()
			return m, nil
		case "up":
			m.sandboxIdx = (m.sandboxIdx - 1 + len(sandboxModes())) % len(sandboxModes())
			return m, nil
		case "down":
			m.sandboxIdx = (m.sandboxIdx + 1) % len(sandboxModes())
			return m, nil
		case "enter", "ctrl+m":
			m.applySandbox(m.sandboxIdx)
			return m, nil
		}
		return m, nil // modal: swallow everything else
	}
	// The /model picker owns the keyboard while open: ↑/↓ move the
	// stage's highlight, Enter advances (list) or commits both switches
	// (effort), Esc cancels — or steps back a stage. ctrl+c still quits.
	if m.modelOpen {
		switch k {
		case "ctrl+c":
			return m.ctrlCQuit()
		case "esc":
			if m.modelStage == modelStageEffort {
				m.modelStage = modelStageList // step back to the model list
				return m, nil
			}
			m.modelOpen = false
			m.resize()
			return m, nil
		case "up", "down":
			if n := m.modelRows(); n > 0 {
				delta := 1
				if k == "up" {
					delta = -1
				}
				if m.modelStage == modelStageEffort {
					m.modelEffortIdx = (m.modelEffortIdx + delta + n) % n
				} else {
					m.modelIdx = (m.modelIdx + delta + n) % n
				}
			}
			return m, nil
		case "enter", "ctrl+m":
			if m.modelStage == modelStageList {
				m.modelEnter()
			} else {
				m.modelCommit()
			}
			return m, nil
		}
		return m, nil // modal: swallow everything else
	}
	// The /models manager owns the keyboard while open: list stages
	// navigate with ↑/↓ and advance on Enter; the key stage is a text
	// input (typing/paste fill the key buffer); Esc steps back a stage.
	// ctrl+c still quits.
	if m.modelsOpen {
		switch k {
		case "ctrl+c":
			return m.ctrlCQuit()
		case "esc":
			m.modelsBack()
			return m, nil
		}
		if m.modelsStage == modelsStageKey {
			switch k {
			case "enter", "ctrl+m":
				m.modelsEnter()
			case "backspace":
				m.modelsKeyBackspace()
			case "ctrl+u":
				m.keyBuf = ""
			default:
				if msg.Text != "" {
					m.modelsKeyType(msg.Text)
				}
			}
			return m, nil
		}
		switch k {
		case "up", "down":
			if n := m.modelsRows(); n > 0 {
				delta := 1
				if k == "up" {
					delta = -1
				}
				switch m.modelsStage {
				case modelsStageVendor:
					m.vendorIdx = (m.vendorIdx + delta + n) % n
				case modelsStageModel:
					m.modelsIdx = (m.modelsIdx + delta + n) % n
				case modelsStageConfirm:
					m.confirmIdx = (m.confirmIdx + delta + n) % n
				}
			}
			return m, nil
		case "enter", "ctrl+m":
			m.modelsEnter()
			return m, nil
		}
		return m, nil // modal: swallow everything else
	}
	// The /mcp overlay owns the keyboard while open: the list navigates
	// with ↑/↓ plus letter hotkeys; the edit screen is a multi-field
	// form (↑/↓ move focus, Enter cycles choices or commits).
	if m.mcpOpen {
		return m.handleMcpKey(msg)
	}
	// The /agents overlay owns the keyboard while open (same modal
	// contract as /mcp).
	if m.agentsOpen {
		return m.handleAgentsKey(msg)
	}
	// The /config panel owns the keyboard while open.
	if m.cfgOpen {
		return m.handleConfigKey(msg)
	}
	// The /memory overlay owns the keyboard while open.
	if m.memOpen {
		return m.handleMemoryKey(msg)
	}
	// The /rewind overlay owns the keyboard while open.
	if m.rewindOpen {
		return m.handleRewindKey(msg)
	}
	// ctrl+v / alt+v / ctrl+shift+v: a clipboard IMAGE (screenshot,
	// copied image file) becomes a pending attachment with a
	// placeholder in the input. A miss arms the bounded watch loop
	// (clipboard_image.go) — the common flow is triggering FIRST and
	// taking the screenshot after — and still falls through to the
	// textarea's normal text paste. alt+v is the reliable trigger on
	// Windows: Windows Terminal & co. swallow ctrl+v as their own paste
	// action (the key never reaches the app). The next trigger re-arms
	// the loop.
	if (k == "ctrl+v" || k == "alt+v" || k == "ctrl+shift+v") && m.clipWatch {
		if m.attachClipboardImage() {
			return m, nil
		}
		tick := m.armClipLoop()
		m.appendBlock(dimStyle.Render(i18n.Tf("tui.main.clipWatch", int(clipLoopWindow/time.Second))))
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.updatePalette()
		m.resize()
		return m, tea.Batch(tick, cmd)
	}
	// "@" on a fresh token opens the file picker (the character never
	// reaches the input).
	if k == "@" && m.atTokenStart() {
		return m, m.openPicker()
	}
	// The completion palette owns navigation keys while open: ↑/↓ move
	// the highlight, Tab accepts, Esc dismisses, Enter accepts unless
	// the typed text already IS a full command (then it falls through
	// to submit).
	if m.paletteOpen {
		switch k {
		case "up":
			m.paletteIdx = (m.paletteIdx - 1 + len(m.paletteHits)) % len(m.paletteHits)
			return m, nil
		case "down":
			m.paletteIdx = (m.paletteIdx + 1) % len(m.paletteHits)
			return m, nil
		case "tab":
			m.acceptPalette()
			return m, nil
		case "esc":
			m.paletteEsc = m.input.Value() // stay closed while this text stands
			m.paletteOpen = false
			m.resize()
			return m, nil
		case "enter", "ctrl+m":
			if !m.paletteExactMatch() {
				m.acceptPalette()
				return m, nil
			}
			m.paletteOpen = false // exact command: submit below
		}
	}
	switch k {
	case "ctrl+c":
		if m.running {
			m.cancelRun() // first ctrl+c aborts the run, like the REPL
			return m, nil
		}
		return m.ctrlCQuit()
	case "ctrl+o":
		// Expand/collapse all thinking blocks.
		m.expandThinking = !m.expandThinking
		m.rebuildRendered()
		m.syncViewport()
		return m, nil
	case "shift+tab":
		// Cycle the session sandbox mode (read-only → workspace-write →
		// danger-full-access). Only reached in the normal input state:
		// overlays and approval/plan reviews claim shift+tab for button
		// navigation earlier in Update; with the completion palette
		// open the key stays with the input.
		if m.paletteOpen {
			break
		}
		m.cycleSandbox()
		return m, nil
	case "esc":
		if m.running {
			m.cancelRun()
			return m, nil
		}
		if m.sel.active && !m.sel.zeroWidth() {
			m.sel = selection{} // idle esc doubles as "deselect"
			return m, nil
		}
		return m, nil
	case "enter", "ctrl+m":
		return m.submit()
	case "pgup":
		m.vp.pageUp()
		return m, nil
	case "pgdn", "pgdown": // bubbletea names PageDown "pgdown"
		m.vp.pageDown()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.updatePalette()
	m.resize()
	return m, cmd
}

// answerKey feeds a single keystroke into a pending tool/sandbox
// approval as an accelerator. Returns false when the key is not one of
// the prompt's options. (Plan reviews never route here — they answer
// through buttons, see answerPlanButton.)
func (m *model) answerKey(k string) bool {
	req := m.pending
	for _, b := range approvalButtons(req.kind) {
		if b.Key == k {
			m.answerApproval(k)
			return true
		}
	}
	return false
}

// answerApproval sends one option letter to the blocked agent
// goroutine and echoes the button's label into the transcript.
func (m *model) answerApproval(letter string) {
	req := m.pending
	label := letter
	for _, b := range approvalButtons(req.kind) {
		if b.Key == letter {
			label = b.Label
		}
	}
	req.answer <- letter
	m.appendBlock(dimStyle.Render(fmt.Sprintf("(%s: %s)", req.kind, label)))
	m.pending = nil
	m.resize()
}

// planPage sizes one scroll step: a line for the arrows, a full window
// for the page keys.
func (m *model) planPage(k string) int {
	if k == "pgup" || k == "pgdn" || k == "pgdown" {
		_, budget := m.planWindow()
		return budget
	}
	return 1
}

// scrollPlan moves the plan body window, clamped to its bounds.
func (m *model) scrollPlan(delta int) {
	lines, budget := m.planWindow()
	limit := len(lines) - budget
	if limit < 0 {
		limit = 0
	}
	m.planScroll = min(limit, max(0, m.planScroll+delta))
}

// answerPlanButton sends the focused plan-review action: 批准执行
// approves, 打回修订 turns the typed draft (or a generic note) into
// revision feedback, 拒绝 declines without feedback.
func (m *model) answerPlanButton() {
	ans := ""
	switch m.planBtn {
	case 0:
		ans = "y"
	case 1:
		fb := strings.TrimSpace(m.input.Value())
		if fb == "" {
			fb = i18n.T("tui.render.planDefaultFeedback")
		}
		ans = "feedback:" + fb
		m.input.Reset()
	default:
		ans = "n"
	}
	m.pending.answer <- ans
	m.appendBlock(dimStyle.Render(i18n.Tf("tui.render.planEcho", planButtons()[m.planBtn])))
	m.pending = nil
	m.resize()
}

func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" && len(m.clipImgs) == 0 {
		return m, nil
	}
	// Plan review: typed text is ALWAYS revision feedback — approval and
	// rejection only happen through the buttons (typing "y" must never
	// confirm by accident).
	if m.pending != nil && m.pending.kind == "plan" {
		m.pending.answer <- "feedback:" + text
		m.appendBlock(dimStyle.Render("(plan review: " + truncate(text, 60) + ")"))
		m.pending = nil
		m.input.Reset()
		m.resize()
		return m, nil
	}
	if m.pending != nil {
		return m, nil
	}
	m.input.Reset()
	m.updatePalette()
	m.resize()
	// $name skill invocations and the built-in prompt commands
	// (/commit, ...) are NOT UI commands — they flow to the prompt path
	// and expand there. Every other "/" line is a UI command. (The
	// legacy /skill:name spelling is excepted the same way.)
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "/skill:") && !cli.IsPromptCommand(text) {
		if m.running {
			m.queued = append(m.queued, text)
			m.appendBlock(dimStyle.Render("(queued command for after this run: " + text + ")"))
			return m, nil
		}
		m.runCommand(text)
		if m.quit {
			return m, tea.Quit
		}
		return m, nil
	}
	// Attachments whose placeholder survived editing ride the prompt;
	// a deleted placeholder drops its attachment.
	kept, _ := splitReferenced(text, m.clipImgs)
	imgs := imageBlocks(kept)
	if text == "" && len(imgs) == 0 {
		m.clipImgs = nil // stale attachments (placeholders deleted)
		return m, nil
	}
	if m.running {
		// A prompt command (/commit) must not steer the in-flight run —
		// the canned workflow needs a turn of its own, so queue it like
		// the UI commands above.
		if cli.IsPromptCommand(text) {
			m.queued = append(m.queued, text)
			m.appendBlock(dimStyle.Render("(queued command for after this run: " + text + ")"))
			return m, nil
		}
		// Steering is text-only (pi's steering messages): strip the
		// placeholders out of the steered text and keep the attachments
		// pending for the next real prompt.
		m.clipImgs = kept
		m.restorePlaceholders()
		steer := strings.TrimSpace(stripPlaceholders(text, kept))
		if len(kept) > 0 {
			m.appendBlock(dimStyle.Render(fmt.Sprintf("(%d image(s) kept — sent with the next prompt)", len(kept))))
		}
		if steer == "" {
			return m, nil
		}
		m.app.Steer(steer)
		m.appendBlock(dimStyle.Render("(steered: " + truncate(steer, 60) + ")"))
		return m, nil
	}
	m.clipImgs = nil
	m.disarmClipLoop() // the prompt left; a late image must not attach
	m.ensureSpacer()
	m.addSpacer() // turn separator: a double gap before the echoed prompt
	// The ✨ marker is an emoji codepoint: it renders in its own gold
	// color (ignoring SGR) and takes two cells — which lands the text
	// right at the gutter column.
	m.appendBlock(userStyle.Render("✨ " + text))
	m.startRun(text, imgs...)
	return m, m.spinTick() // drives the tail line's animation
}

// restorePlaceholders re-fills the input with the kept attachments'
// placeholders (after a mid-run steer consumed the text but not the
// images).
func (m *model) restorePlaceholders() {
	if len(m.clipImgs) == 0 {
		return
	}
	var sb strings.Builder
	for i, c := range m.clipImgs {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(c.placeholder())
	}
	m.input.SetValue(sb.String())
	m.input.CursorEnd()
}

func (m *model) runCommand(line string) {
	// /new starts a fresh session in place: the app rebinds to a new
	// session file and the transcript view resets to the welcome
	// banner (TUI-interactive; the REPL gets a notice via App.Command).
	if line == "/new" {
		if m.running {
			m.appendBlock(noteLine(i18n.T("tui.main.noNewSessionWhileRunning")))
			return
		}
		m.newSession()
		return
	}
	// /sandbox is TUI-only (the mode picker overlay lives here).
	if line == "/sandbox" {
		if m.running {
			m.appendBlock(noteLine(i18n.T("tui.main.noSandboxWhileRunning")))
			return
		}
		m.openSandbox()
		return
	}
	// /mcp is TUI-interactive: the manager overlay configures, toggles,
	// and deletes servers — effective immediately. No running guard:
	// the MCP hooks are mutex-serialized and the agent loop re-reads
	// the tool set per request, so mid-run changes are safe (that is
	// the dynamic part of the feature).
	if line == "/mcp" {
		m.openMcpManager()
		return
	}
	// /agents is TUI-interactive: the manager overlay edits definitions.
	// No running guard: the task tool resolves definitions per call, so
	// mid-run saves apply to the next delegation.
	if line == "/agents" {
		m.openAgentsManager()
		return
	}
	// /config is TUI-interactive: the basic-configuration panel.
	if line == "/config" {
		m.openConfigPanel()
		return
	}
	// /memory is TUI-interactive: the long-term memory manager. The
	// extract sub-command must NOT reach App.Command — its synchronous
	// model call would freeze the UI loop.
	if line == "/memory" {
		m.openMemoryManager()
		return
	}
	if line == "/memory extract" {
		m.memExtractAsync()
		return
	}
	// /rewind is TUI-interactive: the checkpoint browser. The restore
	// forms run host-side (no model call), safe to run inline.
	if line == "/rewind" {
		m.openRewindManager()
		return
	}
	if rest, ok := strings.CutPrefix(line, "/rewind "); ok {
		out := m.app.CheckpointRestoreText(rest)
		m.appendBlock(noteLine(out))
		return
	}
	// /model opens the picker (TUI-interactive); "/model <name>" falls
	// through to App.Command and switches directly.
	if line == "/model" {
		if m.running {
			m.appendBlock(noteLine(i18n.T("tui.main.noModelWhileRunning")))
			return
		}
		m.openModelPicker()
		return
	}
	// /models is TUI-interactive (the four-stage manager overlay lives
	// here); the REPL prints the catalog through App.Command instead.
	if line == "/models" {
		if m.running {
			m.appendBlock(noteLine(i18n.T("tui.main.noModelCfgWhileRunning")))
			return
		}
		m.openModelManager()
		return
	}
	out, done, err := m.app.Command(line)
	if out != "" {
		m.appendBlock(out)
	}
	if err != nil {
		m.appendBlock(errStyle.Render("error: " + err.Error()))
	}
	if done {
		m.quit = true
	}
	m.refreshUsage()
}

// newSession swaps the app onto a fresh session (/new) and resets
// the transcript view: blocks, their scroller mirror, the tool-call
// index, and any selection/scroll state all belong to the OLD
// conversation. The new view opens with the welcome banner naming the
// new session id, plus a note on how to resume the old one.
func (m *model) newSession() {
	oldID, newID, err := m.app.NewSession()
	if err != nil {
		m.appendBlock(errStyle.Render("error: " + err.Error()))
		return
	}
	m.blocks = nil
	m.blockOff = nil
	m.syncedBlocks = 0
	m.toolCalls = map[string]int{}
	m.live = nil
	m.dirty = false
	m.sel = selection{}
	h := m.vp.Height()
	m.vp = scroller{height: h, pinned: true}
	prov, modelID := m.app.CurrentModel()
	m.appendBlock(welcomeBanner(prov, modelID, m.app.CWD, newID))
	m.addSpacer()
	m.appendBlock(noteLine(i18n.Tf("tui.main.newSessionStarted", oldID)))
	m.refreshUsage()
}

func (m *model) startRun(prompt string, images ...llm.Block) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.running = true
	m.phase = phaseRequesting // request in flight until the model answers
	m.spinFrame = 0
	m.syncTail()
	ui := m.ui
	go func() {
		// A panic in the agent goroutine would crash the process with no
		// visible trace (alt screen); fold it into the event stream.
		defer func() {
			if r := recover(); r != nil {
				ui <- runDoneMsg{err: fmt.Errorf("agent panic: %v\n%s", r, debug.Stack())}
			}
		}()
		out := make(chan agent.Event, 256)
		done := make(chan error, 1)
		go func() { done <- m.app.Run(ctx, out, prompt, images...) }()
		for ev := range out {
			ui <- agentEventMsg{ev: ev}
		}
		ui <- runDoneMsg{err: <-done}
	}()
}

func (m *model) cancelRun() {
	if m.cancel != nil {
		m.cancel()
		m.appendBlock(dimStyle.Render("(aborted)"))
	}
}

// ---------------------------------------------------------------------------
// Mouse text selection (left-drag over the transcript; right click
// copies). The terminal's native selection is suppressed by CellMotion
// mouse capture, so the TUI owns selection itself.
// ---------------------------------------------------------------------------

// selection is one in-progress transcript selection. Lines are
// ABSOLUTE transcript line indices (stable across scrolling); columns
// are cell offsets within the wrapped line (ANSI-aware).
type selection struct {
	anchorLine, anchorCol int
	line, col             int // drag head
	active                bool
}

// normalized orders the endpoints lexicographically.
func (s selection) normalized() (l0, c0, l1, c1 int) {
	if s.line < s.anchorLine || (s.line == s.anchorLine && s.col < s.anchorCol) {
		return s.line, s.col, s.anchorLine, s.anchorCol
	}
	return s.anchorLine, s.anchorCol, s.line, s.col
}

// zeroWidth reports a plain click (no drag): it renders nothing and
// copies nothing.
func (s selection) zeroWidth() bool {
	l0, c0, l1, c1 := s.normalized()
	return l0 == l1 && c0 >= c1
}

// selectionPoint maps a screen cell to an absolute (line, col)
// selection point. y is clamped into the viewport, x into the line's
// cell width — so clicking past end-of-line selects to its end.
func (m *model) selectionPoint(x, y int) (int, int) {
	line := m.vp.firstVisible() + min(max(y, 0), m.vp.Height()-1)
	if line >= m.vp.total() {
		line = m.vp.total() - 1
	}
	if line < 0 {
		return 0, 0
	}
	ln, _ := m.vp.lineAt(line)
	return line, min(max(x, 0), ansi.StringWidth(ln))
}

func (m *model) beginSelection(x, y int) {
	line, col := m.selectionPoint(x, y)
	m.sel = selection{anchorLine: line, anchorCol: col, line: line, col: col, active: true}
}

func (m *model) extendSelection(x, y int) {
	m.sel.line, m.sel.col = m.selectionPoint(x, y)
}

// selectionText extracts the selected wrapped lines as plain text:
// endpoint column cuts, ANSI stripped, trailing blanks trimmed. The
// gutter dot stays — a native terminal selection copies it too.
func (m *model) selectionText() string {
	l0, c0, l1, c1 := m.sel.normalized()
	var out []string
	for i := l0; i <= l1 && i < m.vp.total(); i++ {
		ln, ok := m.vp.lineAt(i)
		if !ok {
			continue
		}
		a, b := 0, ansi.StringWidth(ln)
		if i == l0 {
			a = min(c0, b)
		}
		if i == l1 {
			b = min(c1, b)
		}
		if a >= b {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(ansi.Strip(ansi.Cut(ln, a, b)), " "))
	}
	return strings.Join(out, "\n")
}

// copySelection writes the active selection to the clipboard (right
// click). A zero-width or empty selection just clears. The outcome is
// reported as a transient top-right toast.
func (m *model) copySelection() tea.Cmd {
	if !m.sel.active {
		return nil
	}
	text := m.selectionText()
	m.sel = selection{}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if !writeClipboardTextFn(text) {
		return m.showToast(i18n.T("tui.main.copyFail"))
	}
	return m.showToast(i18n.Tf("tui.main.copiedLines", strings.Count(text, "\n")+1))
}

// toastInfo is one transient top-right notification.
type toastInfo struct {
	text string
	id   int
}

// ctrlCQuit gates quitting behind a double press: the first ctrl+c
// arms the guard and floats a hint toast; a second press inside
// quitArmTTL quits. Used by every ctrl+c quit site (global handler and
// all overlays) so a stray keypress never kills the session.
func (m model) ctrlCQuit() (tea.Model, tea.Cmd) {
	if m.quitArmed {
		m.quitArmed = false
		return m, tea.Quit
	}
	m.quitArmed = true
	m.quitSeq++
	id := m.quitSeq
	disarm := tea.Tick(quitArmTTL, func(time.Time) tea.Msg { return quitDisarmMsg{id: id} })
	return m, tea.Batch(m.showToast(i18n.T("tui.quit.confirmHint")), disarm)
}

// showToast floats text at the top-right corner for toastTTL. A newer
// toast replaces an older one; each expiry tick carries its own id so
// a stale timer never clears the current toast.
func (m *model) showToast(text string) tea.Cmd {
	m.toastSeq++
	m.toast = &toastInfo{text: text, id: m.toastSeq}
	id := m.toastSeq
	return tea.Tick(toastTTL, func(time.Time) tea.Msg { return toastExpireMsg{id: id} })
}

// compositeToast floats the toast over the top-right corner of the
// first screen line: padded out when the line leaves room, otherwise
// it covers the line's tail for the toast's lifetime — a terminal view
// is plain text, so covering is the closest thing to floating.
func (m model) compositeToast(v string) string {
	if m.toast == nil {
		return v
	}
	parts := strings.SplitN(v, "\n", 2)
	first := parts[0]
	toast := toastStyle.Render(m.toast.text)
	room := m.width - ansi.StringWidth(toast)
	if room < 1 {
		return v // degenerate width: no toast beats a broken layout
	}
	if fw := ansi.StringWidth(first); fw < room {
		parts[0] = first + strings.Repeat(" ", room-fw) + toast
	} else {
		parts[0] = ansi.Truncate(first, room-1, "") + " " + toast
	}
	return strings.Join(parts, "\n")
}

// renderViewport paints the scroller window with the active selection
// highlighted. The cut/stitch is ANSI-aware, so styled lines keep
// their colors outside the selected span.
func (m model) renderViewport() string {
	v := m.vp.view()
	if !m.sel.active || m.sel.zeroWidth() {
		return v
	}
	l0, c0, l1, c1 := m.sel.normalized()
	base := m.vp.firstVisible()
	rows := strings.Split(v, "\n")
	for y, ln := range rows {
		idx := base + y
		if idx < l0 || idx > l1 || idx >= m.vp.total() {
			continue
		}
		w := ansi.StringWidth(ln)
		a, b := 0, w
		if idx == l0 {
			a = min(c0, w)
		}
		if idx == l1 {
			b = min(c1, w)
		}
		if a >= b {
			continue
		}
		rows[y] = ansi.Cut(ln, 0, a) + selStyle.Render(ansi.Cut(ln, a, b)) + ansi.Cut(ln, b, w)
	}
	return strings.Join(rows, "\n")
}

// renderEvent folds one agent event into the transcript (mirrors the
// cli.Renderer's split: text/thinking stream, tools and errors are
// one-line blocks). Deltas only touch the shared stream buffer and the
// dirty flag — re-layout waits for the flush tick.
func (m *model) renderEvent(ev agent.Event) {
	// Any response-side event moves the tail line from Requesting... to
	// Working... (the model answered; tools count as work too).
	if m.phase == phaseRequesting {
		switch ev.Type {
		case agent.EvLLM, agent.EvToolStart, agent.EvToolProgress, agent.EvTurnEnd, agent.EvAssistant:
			m.phase = phaseWorking
			m.syncTail()
		}
	}
	switch ev.Type {
	case agent.EvLLM:
		if ev.LLM == nil {
			return
		}
		switch ev.LLM.Type {
		case llm.EventTextDelta:
			m.beginLive("text")
			// Wrap at the gutter-adjusted width: every consumer prefixes
			// the 3-cell gutter, so lines cached at the full width would
			// overflow by gutterText cells on the committed re-wrap and
			// leave orphan 1-3 cell rows.
			m.live.appendText(ev.LLM.Delta, max(1, m.width-gutterText))
			m.dirty = true // synced on the flush tick, not per delta
		case llm.EventThinkingDelta:
			m.beginLive("thinking")
			m.live.b.WriteString(ev.LLM.Delta)
			m.dirty = true
		case llm.EventTextEnd, llm.EventThinkingEnd:
			m.flushLive()
		}
	case agent.EvToolStart:
		m.flushLive()
		if ev.Call != nil {
			if m.toolCalls == nil {
				m.toolCalls = map[string]int{}
			}
			// The index is recorded AFTER the leading spacer: ensureSpacer
			// may append one, and the row lands after it — recording first
			// would point EvToolEnd/Progress at the spacer block and
			// overwrite the blank line in place.
			m.ensureSpacer() // gap between tool rows, parallel batches included
			m.toolCalls[ev.Call.ID] = len(m.blocks)
			m.appendBlock(toolRowView(dimStyle, toolRowName(ev.Call), toolSummary(ev.Call.Name, ev.Call.Arguments), m.width))
		}
	case agent.EvToolProgress:
		// A long-running tool (the sub-agent delegate) rewrites its row
		// in place with the live status line.
		if ev.Call == nil || ev.Progress == "" {
			return
		}
		if idx, known := m.toolCalls[ev.Call.ID]; known {
			row := toolRowView(dimStyle, toolRowName(ev.Call), toolSummary(ev.Call.Name, ev.Call.Arguments), m.width) +
				"\n" + dimStyle.Render(gutterPad+"◐ "+truncate(ev.Progress, max(20, m.width-gutterText-2)))
			m.updateBlock(idx, row)
		}
	case agent.EvToolEnd:
		if ev.Call == nil || ev.Result == nil {
			return
		}
		dot := dotOK
		if ev.Result.IsError {
			dot = dotFail
			if isSandboxDenial(ev.Result) {
				dot = dotSandbox
			}
		}
		// The settled row carries the detail preview under it (diff for
		// edit, content for write, output head for bash/read/ls/grep):
		// what the tool DID, not just that it ran.
		view := toolRowView(dot, toolRowName(ev.Call), toolSummary(ev.Call.Name, ev.Call.Arguments), m.width)
		if detail := toolDetail(ev.Call.Name, ev.Call.Arguments, ev.Result, m.width); len(detail) > 0 {
			view += "\n" + strings.Join(detail, "\n")
		}
		idx, known := -1, false
		if m.toolCalls != nil {
			if idx, known = m.toolCalls[ev.Call.ID]; known {
				delete(m.toolCalls, ev.Call.ID)
			}
		}
		if known {
			m.updateBlock(idx, view)
		} else {
			m.ensureSpacer()
			m.appendBlock(view)
		}
		if ev.Result.IsError {
			msg := resultText(ev.Result)
			if msg == "" {
				msg = i18n.Tf("tui.main.toolFailed", capitalize(ev.Call.Name))
			}
			// Multi-line shell errors (Windows cmd's especially) render as
			// a hanging-indented excerpt — never a single line whose
			// continuations fall out of the gutter onto column 0.
			m.appendBlock(strings.Join(errExcerpt(msg, m.width), "\n"))
		}
		// A committed plan update renders the checklist (Claude Code's
		// todo display): the tool row says the call ran, the block is
		// the human-readable progress.
		if ev.Call.Name == plantrack.UpdateToolName && !ev.Result.IsError {
			if plan, ok := m.app.PlanState(); ok {
				m.ensureSpacer()
				m.appendBlock(planBlock(plan, m.width))
			}
		}
	case agent.EvAgentError:
		if ev.Err != nil {
			m.ensureSpacer()
			m.appendBlock(errStyle.Render("error: "+ev.Err.Error()) + "\n")
		}
	}
}

// beginLive switches the streaming buffer's block kind, flushing the
// previous one on a boundary (thinking → text and back).
func (m *model) beginLive(kind string) {
	if m.live == nil || m.live.kind != kind {
		m.flushLive()
		m.live = &streamBuf{kind: kind}
	}
}

// addSpacer appends one blank transcript line — the TUI equivalent of
// line height. Spacing LEADS each item (see ensureSpacer): parallel
// tool rows settle in place mid-batch, so a trailing spacer would land
// at the batch's end instead of between the rows.
func (m *model) addSpacer() {
	m.commitBlock(block{rendered: "", spacer: true})
}

// ensureSpacer leaves exactly one blank line before the next item —
// none at transcript start, none doubling up.
func (m *model) ensureSpacer() {
	if len(m.blocks) > 0 && !m.blocks[len(m.blocks)-1].spacer {
		m.addSpacer()
	}
}

// flushLive commits the streaming buffer as a transcript block. Text
// re-renders from the raw buffer (tables align at this point — the
// streaming tail showed raw pipes); thinking crops/expands per the
// toggle. Either way the commit is O(block), never O(transcript).
func (m *model) flushLive() {
	lv := m.live
	m.live = nil
	m.syncTail() // the status line survives flushes mid-run
	if lv == nil || lv.b.Len() == 0 {
		return
	}
	if lv.kind == "thinking" {
		m.ensureSpacer()
		m.commitBlock(block{rendered: m.renderThinking(lv.text()), think: lv.text(), isThink: true})
		return
	}
	m.ensureSpacer()
	raw := lv.text()
	m.commitBlock(block{rendered: m.renderAssistantText(raw), text: raw, isText: true})
}

// renderAssistantText renders one committed assistant text block: pipe
// tables become aligned tables (renderTablesInText), everything else
// hard-wraps at the gutter-adjusted width — the same visuals the
// streaming incremental cache showed, plus the table upgrade. Width
// changes re-render from the raw text (rebuildRendered).
func (m *model) renderAssistantText(raw string) string {
	w := max(1, m.width-gutterText)
	norm := strings.ReplaceAll(raw, "\r\n", "\n")
	body := strings.Join(renderTablesInText(norm, w), "\n")
	return gutterView(dotBody, strings.Join(wrapText(body, w), "\n"))
}

// appendBlock commits a pre-rendered block and wraps it once — O(block),
// never O(transcript).
func (m *model) appendBlock(s string) {
	m.commitBlock(block{rendered: s})
}

// commitBlock finalizes a block: wrap if not pre-wrapped, cache the
// lines, append them to the scroller, and record the block's line
// offset for in-place updates.
func (m *model) commitBlock(b block) {
	if b.lines == nil {
		b.lines = wrapText(b.rendered, max(1, m.width))
	}
	m.blockOff = append(m.blockOff, len(m.vp.lines))
	m.blocks = append(m.blocks, b)
	m.vp.appendLines(b.lines...)
	m.syncedBlocks = len(m.blocks)
}

// updateBlock re-renders one committed block in place (a settled tool
// call recolors its status dot) and splices the scroller lines.
func (m *model) updateBlock(idx int, rendered string) {
	if idx < 0 || idx >= len(m.blocks) || idx >= len(m.blockOff) {
		return
	}
	blocks := make([]block, len(m.blocks))
	copy(blocks, m.blocks)
	blocks[idx].rendered = rendered
	old := len(blocks[idx].lines)
	blocks[idx].lines = wrapText(rendered, max(1, m.width))
	m.blocks = blocks
	off := m.blockOff[idx]
	m.vp.replaceLines(off, old, blocks[idx].lines)
	if delta := len(blocks[idx].lines) - old; delta != 0 {
		for i := idx + 1; i < len(m.blockOff); i++ {
			m.blockOff[i] += delta
		}
	}
}

// isSandboxDenial reports whether a failed result is a sandbox refusal
// (orange dot) rather than an ordinary error (red).
func isSandboxDenial(res *agent.ToolResult) bool {
	return strings.Contains(strings.ToLower(resultText(res)), "sandbox")
}

// resultText flattens a tool result's text blocks.
func resultText(res *agent.ToolResult) string {
	var b strings.Builder
	for _, blk := range res.Content {
		if blk.Kind == llm.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// rebuildRendered re-renders thinking blocks (expand toggle / width
// change) and assistant text blocks (tables re-align to the new
// width), re-wraps every block, and resets the scroller wholesale.
// O(transcript) — but only on width changes and ctrl+o, never on the
// streaming path. Copies the block slice before mutating entries:
// stale model copies share the backing array.
func (m *model) rebuildRendered() {
	blocks := make([]block, len(m.blocks))
	copy(blocks, m.blocks)
	w := max(1, m.width)
	var all []string
	m.blockOff = m.blockOff[:0]
	for i := range blocks {
		if blocks[i].isThink {
			blocks[i].rendered = m.renderThinking(blocks[i].think)
		} else if blocks[i].isText {
			blocks[i].rendered = m.renderAssistantText(blocks[i].text)
		}
		blocks[i].lines = wrapText(blocks[i].rendered, w)
		m.blockOff = append(m.blockOff, len(all))
		all = append(all, blocks[i].lines...)
	}
	m.blocks = blocks
	m.vp.setLines(all)
	m.syncedBlocks = len(blocks)
}

// syncViewport pushes new committed block lines and the wrapped live
// tail into the scroller — O(new content), never O(transcript).
func (m *model) syncViewport() {
	for i := m.syncedBlocks; i < len(m.blocks); i++ {
		if b := &m.blocks[i]; b.lines == nil { // blocks injected without a cache (tests)
			b.lines = wrapText(b.rendered, max(1, m.width))
		}
	}
	if len(m.blockOff) != len(m.blocks) { // re-align offsets (injected blocks)
		m.blockOff = m.blockOff[:0]
		off := 0
		for _, b := range m.blocks {
			m.blockOff = append(m.blockOff, off)
			off += len(b.lines)
		}
	}
	for i := m.syncedBlocks; i < len(m.blocks); i++ {
		m.vp.appendLines(m.blocks[i].lines...)
	}
	m.syncedBlocks = len(m.blocks)
	m.syncTail()
	m.dirty = false
}

// syncTail refreshes the transcript tail: the streaming buffer, then —
// one blank line below the content — the work-status line while a run
// is in flight (yellow star + Requesting.../Working...).
func (m *model) syncTail() {
	tail := m.liveViewLines()
	if m.phase != phaseIdle {
		tail = append(tail, "", " "+spinStyle.Render(spinFrames[m.spinFrame]+" "+m.phase.label()))
	}
	m.vp.setLive(tail)
}

// liveViewLines renders the streaming tail as wrapped lines. Text comes
// from the incremental cache under the white content dot (only the
// partial line re-wraps); thinking goes through the capped cropper so a
// long stream costs bounded work per frame (full fidelity once expanded
// or committed). The tail LEADS with one blank line when committed
// content sits above it — spacing leads items (see ensureSpacer), so
// the gap must be visible while streaming, not pop in at flush time
// (which would shift the transcript down mid-run).
func (m *model) liveViewLines() []string {
	lv := m.live
	if lv == nil || lv.b.Len() == 0 {
		return nil
	}
	w := max(1, m.width)
	var body []string
	if lv.kind == "thinking" {
		raw := lv.text()
		if m.expandThinking {
			body = wrapText(m.renderThinking(raw), w)
		} else {
			body = wrapText(m.renderThinkingCapped(raw, thinkingTailBytes), w)
		}
	} else {
		text := gutterView(dotBody, strings.Join(lv.viewLines(max(1, w-gutterText)), "\n"))
		body = wrapText(text, w)
	}
	if len(m.blocks) > 0 && !m.blocks[len(m.blocks)-1].spacer {
		body = append([]string{""}, body...)
	}
	return body
}

// maybeFlushTick arms the streaming frame clock (at most one tick in
// flight). Nil when there's nothing new to draw.
func (m *model) maybeFlushTick() tea.Cmd {
	if !m.dirty || m.flushScheduled {
		return nil
	}
	m.flushScheduled = true
	return tea.Tick(flushInterval, func(time.Time) tea.Msg { return flushTickMsg{} })
}

// resize lays out viewport / approval box / palette / input / status bar.
// Cheap on the hot path (typing/blink): when neither the width nor the
// viewport height changed it does nothing at all — AtBottom alone walks
// every transcript line, and a full re-compose is reserved for width
// changes.
// inputHidden reports whether a keyboard-owning modal has pushed the
// input box out of the layout: the button-driven approval asks (tool/
// sandbox) and the full managers (sandbox/model/models/mcp). The plan
// review KEEPS the input — its typed text is revision feedback — and so
// do the typing-attached popups (palette, file picker), whose input is
// the thing driving them.
func (m *model) inputHidden() bool {
	if m.pending != nil && m.pending.kind != "plan" {
		return true
	}
	return m.sandboxOpen || m.modelOpen || m.modelsOpen || m.mcpOpen || m.agentsOpen || m.cfgOpen || m.memOpen || m.rewindOpen
}

func (m *model) resize() {
	iw := m.width - 2 // input box left/right borders
	if iw < 1 {
		iw = 1
	}
	m.input.SetWidth(iw)
	ih := m.input.Height()
	if ih < 1 {
		ih = 1
	}
	ih += 2 // input box chrome: label rule above + bottom border below
	ah := 0
	if m.pending != nil {
		ah = lipgloss.Height(m.approvalView())
	}
	ph := 0
	if m.paletteOpen && len(m.paletteHits) > 0 {
		ph = lipgloss.Height(m.paletteView())
	}
	kh := 0
	if m.pickerOpen {
		kh = lipgloss.Height(m.pickerView())
	}
	sh := 0
	if m.sandboxOpen {
		sh = lipgloss.Height(m.sandboxView())
	}
	mh := 0
	if m.modelOpen {
		mh = lipgloss.Height(m.modelPickerView())
	}
	gh := 0
	if m.modelsOpen {
		gh = lipgloss.Height(m.modelsView())
	}
	ch := 0
	if m.mcpOpen {
		ch = lipgloss.Height(m.mcpView())
	}
	agh := 0
	if m.agentsOpen {
		agh = lipgloss.Height(m.agentsView())
	}
	cfh := 0
	if m.cfgOpen {
		cfh = lipgloss.Height(m.configView())
	}
	mh2 := 0
	if m.memOpen {
		mh2 = lipgloss.Height(m.memoryView())
	}
	rh := 0
	if m.rewindOpen {
		rh = lipgloss.Height(m.rewindView())
	}
	vh := m.height - ah - ph - kh - sh - mh - gh - ch - agh - cfh - mh2 - rh
	if !m.inputHidden() {
		vh -= ih + 1 // input box + status bar
	}
	if vh < 1 {
		vh = 1
	}
	widthChanged := m.contentWidth != m.width
	heightChanged := vh != m.vp.Height()
	if !widthChanged && !heightChanged {
		return
	}
	wasAtBottom := m.vp.AtBottom()
	m.vp.SetHeight(vh)
	if widthChanged {
		// Width change: thinking blocks re-render, transcript re-wraps.
		m.contentWidth = m.width
		m.rebuildRendered()
		if m.live != nil && m.live.kind == "text" {
			m.live.resetWrap(max(1, m.width-gutterText)) // streaming cache re-wraps too (gutter-adjusted)
		}
		m.syncViewport()
		return
	}
	if wasAtBottom {
		m.vp.GotoBottom() // keep the transcript pinned as the box grows
	}
}

func (m *model) refreshUsage() {
	u := m.app.UsageReport()
	m.usage = &u
}

func (m model) view() tea.View {
	parts := []string{m.renderViewport()}
	if m.pending != nil {
		parts = append(parts, m.approvalView())
	}
	if m.paletteOpen && len(m.paletteHits) > 0 {
		parts = append(parts, m.paletteView())
	}
	if m.pickerOpen {
		parts = append(parts, m.pickerView())
	}
	if m.sandboxOpen {
		parts = append(parts, m.sandboxView())
	}
	if m.modelOpen {
		parts = append(parts, m.modelPickerView())
	}
	if m.modelsOpen {
		parts = append(parts, m.modelsView())
	}
	if m.mcpOpen {
		parts = append(parts, m.mcpView())
	}
	if m.agentsOpen {
		parts = append(parts, m.agentsView())
	}
	if m.cfgOpen {
		parts = append(parts, m.configView())
	}
	if m.memOpen {
		parts = append(parts, m.memoryView())
	}
	if m.rewindOpen {
		parts = append(parts, m.rewindView())
	}
	// The input box AND its status bar leave together when a modal owns
	// the screen.
	showInput := !m.inputHidden()
	inputIdx := len(parts)
	if showInput {
		parts = append(parts, m.inputBoxView(), m.statusView())
	}
	v := tea.NewView(m.compositeToast(strings.Join(parts, "\n")))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion // wheel events for transcript scroll
	v.WindowTitle = "scode"
	// Anchor the hardware cursor at the composer's text position so IME
	// candidate windows pop up at the text end, not wherever the last
	// render pass left the cursor (the status bar path). nil while a
	// modal hides the input — the renderer hides the cursor then.
	if showInput {
		if c := m.input.Cursor(); c != nil {
			top := 0
			for _, p := range parts[:inputIdx] {
				top += lipgloss.Height(p) // "\n" joins are line terminators, not extra rows
			}
			if m.width >= 6 {
				c.Position.X++ // box left border cell
				top++          // the labelled top rule line
			}
			c.Position.Y += top
			v.Cursor = c
		}
	}
	return v
}
