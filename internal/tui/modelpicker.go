package tui

import (
	"fmt"
	"strings"

	"scode/internal/i18n"
)

// /model runs TWO stages in one command: pick the configured
// provider/model pair, then its reasoning effort — Enter on the effort
// commits both together (App.SetModel + App.SetThinkingLevel), so one
// command switches model and thinking in a single flow. A bare
// "/model <name>" in the REPL switches the model directly.

// modelStage is the picker's current step.
type modelStage int

const (
	modelStageList   modelStage = iota // choose the provider/model pair
	modelStageEffort                   // choose the reasoning effort
)

// modelEfforts are the selectable reasoning levels in display order
// ("" follows the provider default); labels via thinkingLabel.
var modelEfforts = []string{"", "off", "low", "medium", "high"}

// openModelPicker loads the choices and highlights the current pair.
func (m *model) openModelPicker() {
	m.modelChoices = m.app.ConfiguredModels()
	if len(m.modelChoices) == 0 {
		m.appendBlock(noteLine(i18n.T("tui.modelpicker.none")))
		return
	}
	prov, mid := m.app.CurrentModel()
	m.modelIdx = 0
	for i, c := range m.modelChoices {
		if c.Provider == prov && c.Model == mid {
			m.modelIdx = i
		}
	}
	m.modelStage = modelStageList
	m.modelOpen = true
	m.resize()
}

// modelRows is the current stage's row count (navigation modulus).
func (m *model) modelRows() int {
	if m.modelStage == modelStageEffort {
		return len(modelEfforts)
	}
	return len(m.modelChoices)
}

// modelEnter advances from the model list to the effort stage, keeping
// the highlighted pair as the pending switch.
func (m *model) modelEnter() {
	m.modelPending = m.modelIdx
	m.modelStage = modelStageEffort
	m.modelEffortIdx = 0
	for i, lv := range modelEfforts {
		if lv == m.app.CurrentThinkingLevel() {
			m.modelEffortIdx = i
		}
	}
}

// modelCommit applies the pending model and the highlighted effort in
// one go, echoing one combined note. The choice ALSO lands in
// settings.json as the defaults, so new sessions and the next launch
// start from it — not just this session (whose own replay comes from
// the session log entries SetModel/SetThinkingLevel persist). Unchanged
// parts stay untouched and unmentioned; a fully unchanged pick closes
// quietly.
func (m *model) modelCommit() {
	m.modelOpen = false
	m.resize()
	var notes []string
	p, mid := m.app.CurrentModel()
	if m.modelPending >= 0 && m.modelPending < len(m.modelChoices) {
		c := m.modelChoices[m.modelPending]
		if c.Provider != p || c.Model != mid {
			np, nMid, err := m.app.SetModel(c.Provider, c.Model)
			if err != nil {
				m.appendBlock(errStyle.Render("error: " + err.Error()))
				return
			}
			p, mid = np, nMid
			notes = append(notes, i18n.Tf("tui.modelpicker.switched", p, mid))
		}
	}
	if m.modelEffortIdx >= 0 && m.modelEffortIdx < len(modelEfforts) {
		if lv := modelEfforts[m.modelEffortIdx]; lv != m.app.CurrentThinkingLevel() {
			if err := m.app.SetThinkingLevel(lv); err != nil {
				m.appendBlock(errStyle.Render("error: " + err.Error()))
				return
			}
			notes = append(notes, i18n.Tf("tui.models.effort", thinkingLabel(lv)))
		}
	}
	// Remember the choice as the settings-level defaults.
	if err := m.app.PersistDefaultModel(p, mid); err != nil {
		m.appendBlock(dimStyle.Render(i18n.Tf("tui.models.defaultModelFail", err)))
	} else if err := m.app.PersistDefaultThinking(modelEfforts[m.modelEffortIdx]); err != nil {
		m.appendBlock(dimStyle.Render(i18n.Tf("tui.models.defaultEffortFail", err)))
	} else {
		notes = append(notes, i18n.T("tui.models.savedDefault"))
	}
	if len(notes) > 0 {
		m.appendBlock(noteLine(strings.Join(notes, " · ")))
	}
}

// modelPickerView renders the current stage inside the overlay.
func (m *model) modelPickerView() string {
	if m.modelStage == modelStageEffort {
		return m.modelEffortView()
	}
	return m.modelListView()
}

// modelListView renders the configured models as a windowed list (a
// long config scrolls with the highlight): rows show the provider /
// model label with the current one marked, plus a key hint.
func (m *model) modelListView() string {
	rows := make([]string, 0, len(m.modelChoices)+1)
	prov, mid := m.app.CurrentModel()

	// Window the list like the palette: enough rows to matter, capped so
	// the box never swallows the screen (rows are double-spaced, so the
	// budget halves); hidden counts collapse into "… N" lines.
	maxRows := max(3, m.height/4-2)
	before, after := 0, 0
	hits := m.modelChoices
	if len(hits) > maxRows {
		start := m.modelIdx - maxRows + 1 // scroll just enough
		if start < 0 {
			start = 0
		}
		if lim := len(hits) - maxRows; start > lim {
			start = lim
		}
		before = start
		after = len(hits) - start - maxRows
		hits = hits[start : start+maxRows]
	}
	if before > 0 {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("  … %d above", before)))
	}
	for i, c := range hits {
		row := c.Label
		if c.Provider == prov && c.Model == mid {
			row += dimStyle.Render(i18n.T("tui.sandbox.currentMark"))
		}
		if i == m.modelIdx-before {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	if after > 0 {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("  … %d more", after)))
	}
	rows = append(rows, dimStyle.Render(i18n.T("tui.modelpicker.listHint")))
	return m.overlayView(i18n.T("tui.modelpicker.listTitle"), overlayList(rows))
}

// modelEffortView renders the second stage: the pending model as a
// header line, then the reasoning levels with the current one marked.
func (m *model) modelEffortView() string {
	header := "  "
	if m.modelPending >= 0 && m.modelPending < len(m.modelChoices) {
		header += m.modelChoices[m.modelPending].Label
	}
	rows := []string{dimStyle.Render(header)}
	cur := m.app.CurrentThinkingLevel()
	for i, lv := range modelEfforts {
		row := thinkingLabel(lv)
		if lv == cur {
			row += dimStyle.Render(i18n.T("tui.sandbox.currentMark"))
		}
		if i == m.modelEffortIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render(i18n.T("tui.modelpicker.effortHint")))
	return m.overlayView(i18n.T("tui.modelpicker.effortTitle"), overlayList(rows))
}
