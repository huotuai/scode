package tui

// /agents: the sub-agent manager overlay. Two stages over agents.json:
// the list (navigate/edit/new/delete) and a multi-field form (name,
// description, model, reasoning effort). Saves take effect on the NEXT
// task call — the delegation tool resolves definitions per call — so
// there is no running guard and no registry churn.

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"scode/internal/config"
	"scode/internal/subagent"
)

type agentsStage int

const (
	agentsStageList agentsStage = iota
	agentsStageEdit
)

// agentForm is the edit-stage state; editing names the spec being
// changed ("" = new).
type agentForm struct {
	editing string
	name    string
	desc    string
	model   string
	effort  string // "" | off | low | medium | high
}

type agentFieldKind int

const (
	agentFieldText agentFieldKind = iota
	agentFieldChoice
	agentFieldAction
)

type agentField struct {
	id    string
	label string
	kind  agentFieldKind
	value string
}

func (f agentForm) fields(models []config.ModelChoice) []agentField {
	effort := f.effort
	if effort == "" {
		effort = "默认"
	}
	model := "(继承会话模型)"
	if f.model != "" {
		model = f.model
		for _, c := range models {
			if c.Provider+":"+c.Model == f.model {
				model = c.Label
				break
			}
		}
	}
	nameLabel := "名称"
	if f.editing != "" {
		nameLabel = "名称(重命名会新建一条)"
	}
	return []agentField{
		{id: "name", label: nameLabel, kind: agentFieldText, value: f.name},
		{id: "desc", label: "描述 · 何时委派给它", kind: agentFieldText, value: f.desc},
		{id: "model", label: "模型(enter 在已激活模型间切换)", kind: agentFieldChoice, value: model},
		{id: "effort", label: "推理强度", kind: agentFieldChoice, value: effort},
		{id: "save", label: "[保存并生效]", kind: agentFieldAction},
	}
}

// cycleAgentModel walks the model choice: inherit → each activated
// model (settings providers) → inherit. A stored key that no longer
// matches any choice (hand-edited file, removed provider) still cycles
// from its position via the inherit slot.
func (m *model) cycleAgentModel() string {
	keys := []string{""}
	for _, c := range m.agentModels {
		keys = append(keys, c.Provider+":"+c.Model)
	}
	cur := m.agentForm.model
	for i, k := range keys {
		if k == cur {
			return keys[(i+1)%len(keys)]
		}
	}
	return keys[1%len(keys)] // unknown key: jump to the first real model
}

// agentsRefresh pulls the definitions for the list view: the merged
// surface (built-ins + overrides) for display, plus which names have a
// file entry (drives the 内置 mark and the delete guard).
func (m *model) agentsRefresh() {
	specs, err := m.app.Agents()
	if err != nil {
		return // keep the previous list; saves surface their own errors
	}
	m.agentUserNames = map[string]bool{}
	for _, s := range specs {
		m.agentUserNames[s.Name] = true
	}
	m.agentsSpecs = subagent.All(specs)
}

// agentIsBuiltin reports whether the idx entry is a pure built-in (no
// agents.json override): shown as 内置, never deleted, editable into
// an override.
func (m *model) agentIsBuiltin(idx int) bool {
	if idx < 0 || idx >= len(m.agentsSpecs) {
		return false
	}
	return subagent.IsBuiltin(m.agentsSpecs[idx].Name) && !m.agentUserNames[m.agentsSpecs[idx].Name]
}

func (m *model) openAgentsManager() {
	m.agentsOpen = true
	m.agentsStage = agentsStageList
	m.agentsConfirmDel = false
	if m.agentsIdx >= len(m.agentsSpecs) {
		m.agentsIdx = 0
	}
	// The form's model picker draws from the ACTIVATED models (the same
	// surface as /model), never free text.
	m.agentModels = m.app.ConfiguredModels()
	m.agentsRefresh()
	m.resize()
}

func (m model) handleAgentsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if m.agentsStage == agentsStageEdit {
		return m.handleAgentsEditKey(msg, k)
	}
	switch k {
	case "esc":
		m.agentsOpen = false
		m.resize()
		return m, nil
	case "up":
		if n := len(m.agentsSpecs); n > 0 {
			m.agentsIdx = (m.agentsIdx - 1 + n) % n
		}
		m.agentsConfirmDel = false
		return m, nil
	case "down":
		if n := len(m.agentsSpecs); n > 0 {
			m.agentsIdx = (m.agentsIdx + 1) % n
		}
		m.agentsConfirmDel = false
		return m, nil
	case "enter", "ctrl+m":
		if idx := m.agentsIdx; idx < len(m.agentsSpecs) {
			s := m.agentsSpecs[idx]
			m.agentForm = agentForm{editing: s.Name, name: s.Name, desc: s.Description, model: s.Model, effort: s.Effort}
			m.agentsStage = agentsStageEdit
			m.agentsEditField = 0
			m.resize()
		}
		return m, nil
	case "n":
		m.agentForm = agentForm{}
		m.agentsStage = agentsStageEdit
		m.agentsEditField = 0
		m.resize()
		return m, nil
	case "d":
		idx := m.agentsIdx
		if idx >= len(m.agentsSpecs) {
			return m, nil
		}
		if m.agentIsBuiltin(idx) {
			m.agentsConfirmDel = false
			return m, m.showToast("内置代理不可删除 — enter 编辑保存后即为自定义覆盖")
		}
		name := m.agentsSpecs[idx].Name
		if !m.agentsConfirmDel {
			m.agentsConfirmDel = true
			return m, nil
		}
		if err := m.app.AgentDelete(name); err != nil {
			return m, m.showToast("agents: " + err.Error())
		}
		m.agentsConfirmDel = false
		m.agentsRefresh()
		if m.agentsIdx >= len(m.agentsSpecs) && m.agentsIdx > 0 {
			m.agentsIdx--
		}
		return m, m.showToast("子代理 " + name + " 已删除")
	}
	return m, nil // modal: swallow everything else
}

// handleAgentsEditKey drives the form: ↑/↓ move focus, Enter cycles
// the choice field or commits on the save row, text lands in the
// focused text field.
func (m model) handleAgentsEditKey(msg tea.KeyPressMsg, k string) (tea.Model, tea.Cmd) {
	fields := m.agentForm.fields(m.agentModels)
	f := fields[min(m.agentsEditField, len(fields)-1)]
	switch k {
	case "esc":
		m.agentsStage = agentsStageList
		m.agentsRefresh()
		m.resize()
		return m, nil
	case "up":
		m.agentsEditField = (m.agentsEditField - 1 + len(fields)) % len(fields)
		return m, nil
	case "down":
		m.agentsEditField = (m.agentsEditField + 1) % len(fields)
		return m, nil
	case "enter", "ctrl+m":
		switch f.id {
		case "model":
			m.agentForm.model = m.cycleAgentModel()
			return m, nil
		case "effort":
			m.agentForm.effort = cycleEffort(m.agentForm.effort)
			return m, nil
		case "save":
			return m.agentCommit()
		}
		return m, nil
	case "backspace":
		if f.kind == agentFieldText {
			m.agentForm.setText(f.id, truncateLastRune(f.value))
		}
		return m, nil
	case "ctrl+u":
		if f.kind == agentFieldText {
			m.agentForm.setText(f.id, "")
		}
		return m, nil
	}
	if msg.Text != "" && f.kind == agentFieldText {
		m.agentForm.setText(f.id, f.value+msg.Text)
	}
	return m, nil
}

// setText writes one text field by id.
func (f *agentForm) setText(id, v string) {
	switch id {
	case "name":
		f.name = v
	case "desc":
		f.desc = v
	case "model":
		f.model = v
	}
}

// cycleEffort walks 默认 → low → medium → high → off → 默认.
func cycleEffort(cur string) string {
	switch cur {
	case "":
		return "low"
	case "low":
		return "medium"
	case "medium":
		return "high"
	case "high":
		return "off"
	default:
		return ""
	}
}

// agentCommit validates and persists the form.
func (m model) agentCommit() (tea.Model, tea.Cmd) {
	f := m.agentForm
	spec := subagent.Spec{
		Name:        strings.TrimSpace(f.name),
		Description: strings.TrimSpace(f.desc),
		Model:       strings.TrimSpace(f.model),
		Effort:      f.effort,
	}
	if err := m.app.AgentSave(spec); err != nil {
		return m, m.showToast("agents: " + err.Error())
	}
	m.agentsStage = agentsStageList
	m.agentsRefresh()
	for i, s := range m.agentsSpecs {
		if s.Name == spec.Name {
			m.agentsIdx = i
		}
	}
	return m, m.showToast("子代理 " + spec.Name + " 已保存 · 下次委派生效")
}

// agentsView renders the current stage.
func (m *model) agentsView() string {
	if m.agentsStage == agentsStageEdit {
		return m.agentsEditView()
	}
	return m.agentsListView()
}

func (m *model) agentsListView() string {
	var rows []string
	for i, s := range m.agentsSpecs {
		model := s.Model
		if model == "" {
			model = "(继承会话模型)"
		}
		effort := s.Effort
		if effort == "" {
			effort = "默认"
		}
		row := s.Name + dimStyle.Render(" · "+model+" · 推理 "+effort)
		if m.agentIsBuiltin(i) {
			row += dimStyle.Render(" · 内置")
		}
		if s.Description != "" {
			row += dimStyle.Render(" · " + truncate(s.Description, 40))
		}
		if i == m.agentsIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	if len(rows) == 0 {
		rows = append(rows, dimStyle.Render("  (无子代理 — 按 n 新建)"))
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · enter 编辑 · n 新建 · d 删除 · esc 关闭"))
	if m.agentsConfirmDel && m.agentsIdx < len(m.agentsSpecs) {
		rows = append(rows, warnStyle.Render("  再按 d 确认删除 "+m.agentsSpecs[m.agentsIdx].Name))
	}
	rows = append(rows, dimStyle.Render("  委派:会话中让模型用 task 工具(子代理独立上下文,只读,互不影响)"))
	return m.overlayView("子代理管理", overlayList(rows))
}

func (m *model) agentsEditView() string {
	f := m.agentForm
	title := "子代理新建"
	if f.editing != "" {
		title = "子代理编辑: " + f.editing
	}
	var rows []string
	for i, field := range f.fields(m.agentModels) {
		row := field.label
		if field.value != "" {
			row += ": " + field.value
		}
		if field.kind == agentFieldChoice {
			row += dimStyle.Render("  (enter 切换)")
		}
		if i == m.agentsEditField {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 字段 · enter 切换选项/保存 · 直接输入文字 · ctrl+u 清空 · esc 返回"))
	return m.overlayView(title, overlayList(rows))
}

// agentsType routes pasted text into the focused form field.
func (m *model) agentsType(s string) {
	fields := m.agentForm.fields(m.agentModels)
	f := fields[min(m.agentsEditField, len(fields)-1)]
	if f.kind == agentFieldText {
		m.agentForm.setText(f.id, f.value+s)
	}
}

// truncateLastRune drops the last rune of s (backspace on a text field).
func truncateLastRune(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return string(r[:len(r)-1])
}
