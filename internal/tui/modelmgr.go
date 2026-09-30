package tui

import (
	"fmt"
	"sort"
	"strings"

	"scode/internal/config"
)

// /models is the model MANAGER (the /model picker only switches between
// already-configured profiles): a four-stage overlay walking the preset
// catalog (config.LoadPresetCatalog) —
//
//	vendor list → API key input → the vendor's model list → confirm
//
// Enter on the confirm stage writes the profile to settings.json
// (providers."vendor:model" via config.UpsertProvider), switches the
// live session onto it, and records it as the default — so /model sees
// the freshly configured model immediately. One vendor carries many
// models; each configures independently (re-run /models for the next
// one, the saved key is offered for reuse). Esc steps back a stage.

// modelsStage is the manager's current step.
type modelsStage int

const (
	modelsStageVendor  modelsStage = iota // choose the vendor group
	modelsStageKey                        // enter (or reuse) the API key
	modelsStageModel                      // choose one of the vendor's preset models
	modelsStageConfirm                    // summary + reasoning strength, Enter commits
)

// openModelManager loads the preset catalog and opens the overlay on
// the vendor list.
func (m *model) openModelManager() {
	cat, err := config.LoadPresetCatalog()
	if err != nil {
		m.appendBlock(errStyle.Render("预设模型目录读取失败: " + err.Error()))
		return
	}
	m.modelsCat = cat
	m.modelsStage = modelsStageVendor
	m.vendorIdx, m.modelsIdx, m.confirmIdx = 0, 0, 0
	m.keyBuf, m.keyExisting = "", ""
	// Preselect the vendor of the current profile ("vendor:model").
	prov, _ := m.app.CurrentModel()
	if i := strings.Index(prov, ":"); i > 0 {
		for j, v := range cat.Vendors {
			if v.ID == prov[:i] {
				m.vendorIdx = j
			}
		}
	}
	m.modelsOpen = true
	m.resize()
}

// modelsVendor returns the highlighted vendor (nil when out of range).
func (m *model) modelsVendor() *config.PresetVendor {
	if m.modelsCat == nil || m.vendorIdx < 0 || m.vendorIdx >= len(m.modelsCat.Vendors) {
		return nil
	}
	return &m.modelsCat.Vendors[m.vendorIdx]
}

// modelsVendorKey scans the freshest settings for an already-saved key
// of this vendor (any "vendor:*" profile with an apiKey).
func (m *model) modelsVendorKey(vendorID string) string {
	s := m.app.Settings
	if fresh, err := config.LoadSettings(); err == nil {
		s = fresh
	}
	names := make([]string, 0, len(s.Providers))
	for name := range s.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.HasPrefix(name, vendorID+":") && s.Providers[name].APIKey != "" {
			return s.Providers[name].APIKey
		}
	}
	return ""
}

// modelsConfigured reports whether the vendor already has any
// configured profile (the vendor-list marker).
func (m *model) modelsConfigured(vendorID string) bool {
	s := m.app.Settings
	if fresh, err := config.LoadSettings(); err == nil {
		s = fresh
	}
	for name := range s.Providers {
		if strings.HasPrefix(name, vendorID+":") {
			return true
		}
	}
	return false
}

// modelsModelConfigured reports whether this exact vendor:model pair is
// already in settings (the model-list marker).
func (m *model) modelsModelConfigured(vendorID, modelID string) bool {
	s := m.app.Settings
	if fresh, err := config.LoadSettings(); err == nil {
		s = fresh
	}
	_, ok := s.Providers[config.ProfileName(vendorID, modelID)]
	return ok
}

// modelsRows is the current stage's navigable row count (the key stage
// is a text input and never routes here).
func (m *model) modelsRows() int {
	switch m.modelsStage {
	case modelsStageVendor:
		if m.modelsCat != nil {
			return len(m.modelsCat.Vendors)
		}
	case modelsStageModel:
		if v := m.modelsVendor(); v != nil {
			return len(v.Models)
		}
	case modelsStageConfirm:
		return len(m.modelsEffortChoices())
	}
	return 0
}

// modelsEffortChoices is the confirm stage's list: the preset model's
// efforts for reasoning models, a single "no reasoning" pseudo-choice
// otherwise.
func (m *model) modelsEffortChoices() []config.PresetEffort {
	v := m.modelsVendor()
	if v == nil || m.modelsIdx < 0 || m.modelsIdx >= len(v.Models) {
		return nil
	}
	pm := v.Models[m.modelsIdx]
	if pm.Reasoning && len(pm.Efforts) > 0 {
		return pm.Efforts
	}
	return []config.PresetEffort{{Level: "", Label: "无推理(直接确认)"}}
}

// modelsEnter advances one stage.
func (m *model) modelsEnter() {
	switch m.modelsStage {
	case modelsStageVendor:
		v := m.modelsVendor()
		if v == nil {
			return
		}
		// Offer the saved key for reuse: the input starts empty and
		// Enter keeps it; typing replaces it.
		m.keyBuf = ""
		m.keyExisting = m.modelsVendorKey(v.ID)
		m.modelsStage = modelsStageKey
	case modelsStageKey:
		v := m.modelsVendor()
		if v == nil {
			return
		}
		m.modelsStage = modelsStageModel
		m.modelsIdx = 0
		// Preselect the pair the session currently runs.
		if prov, mid := m.app.CurrentModel(); strings.HasPrefix(prov, v.ID+":") {
			for i, pm := range v.Models {
				if pm.ID == mid {
					m.modelsIdx = i
				}
			}
		}
	case modelsStageModel:
		m.modelsStage = modelsStageConfirm
		m.confirmIdx = 0
		v := m.modelsVendor()
		if v != nil && m.modelsIdx >= 0 && m.modelsIdx < len(v.Models) {
			pm := v.Models[m.modelsIdx]
			for i, e := range m.modelsEffortChoices() {
				if e.Level == pm.DefaultEffort {
					m.confirmIdx = i
				}
			}
		}
	case modelsStageConfirm:
		m.modelsCommit()
	}
}

// modelsBack steps Esc back one stage (the vendor list closes outright).
func (m *model) modelsBack() {
	switch m.modelsStage {
	case modelsStageConfirm:
		m.modelsStage = modelsStageModel
	case modelsStageModel:
		m.modelsStage = modelsStageKey
	case modelsStageKey:
		m.modelsStage = modelsStageVendor
	default:
		m.modelsOpen = false
		m.resize()
	}
}

// modelsCommit writes the profile to settings, switches the live
// session onto it, applies the chosen reasoning strength, and records
// both as the defaults — the /model picker then lists the new profile.
func (m *model) modelsCommit() {
	m.modelsOpen = false
	m.resize()
	v := m.modelsVendor()
	if v == nil || m.modelsIdx < 0 || m.modelsIdx >= len(v.Models) {
		return
	}
	pm := v.Models[m.modelsIdx]
	key := strings.TrimSpace(m.keyBuf)
	if key == "" {
		key = m.keyExisting // Enter without typing keeps the saved key
	}
	profile := config.ProfileName(v.ID, pm.ID)
	pc := config.ProviderConfig{
		BaseURL:       v.BaseURL,
		APIKey:        key,
		Model:         pm.ID,
		Protocol:      v.Protocol,
		ContextWindow: pm.ContextWindow,
		MaxTokens:     pm.MaxTokens,
		Reasoning:     pm.Reasoning,
		ImageInput:    pm.ImageInput,
	}
	if err := config.UpsertProvider(profile, pc, ""); err != nil {
		m.appendBlock(errStyle.Render("配置写入失败: " + err.Error()))
		return
	}
	var notes []string
	// Switch the live session (the picker guarantee: no run in flight,
	// /models refuses while running).
	p, mid, err := m.app.SetModel(profile, pm.ID)
	if err != nil {
		m.appendBlock(errStyle.Render("模型已配置,但切换失败: " + err.Error()))
		return
	}
	notes = append(notes, "模型已配置并切换 → "+p+" / "+mid)
	effort := ""
	if choices := m.modelsEffortChoices(); m.confirmIdx >= 0 && m.confirmIdx < len(choices) {
		effort = choices[m.confirmIdx].Level
	}
	// The session's strength follows the choice (a non-reasoning model
	// resets it to the provider default, matching the persisted "").
	if effort != m.app.CurrentThinkingLevel() {
		if err := m.app.SetThinkingLevel(effort); err != nil {
			m.appendBlock(errStyle.Render("error: " + err.Error()))
			return
		}
		if pm.Reasoning && effort != "" {
			notes = append(notes, "推理强度 → "+pm.EffortLabel(effort))
		}
	}
	if err := m.app.PersistDefaultModel(p, mid); err != nil {
		m.appendBlock(dimStyle.Render("(默认模型保存失败: " + err.Error() + ")"))
	} else if err := m.app.PersistDefaultThinking(effort); err != nil {
		m.appendBlock(dimStyle.Render("(默认推理强度保存失败: " + err.Error() + ")"))
	} else {
		notes = append(notes, "已存为默认")
	}
	m.appendBlock(noteLine(strings.Join(notes, " · ")))
}

// ---------------------------------------------------------------------------
// key stage input handling
// ---------------------------------------------------------------------------

// modelsKeyType appends pasted/typed text to the key buffer (whitespace
// stripped: keys never contain any).
func (m *model) modelsKeyType(s string) {
	m.keyBuf += strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
}

// modelsKeyBackspace deletes the buffer's last rune.
func (m *model) modelsKeyBackspace() {
	rs := []rune(m.keyBuf)
	if len(rs) > 0 {
		m.keyBuf = string(rs[:len(rs)-1])
	}
}

// maskKey renders a key for display: the head and tail around an
// ellipsis, short keys fully masked.
func maskKey(key string) string {
	rs := []rune(key)
	switch {
	case len(rs) == 0:
		return ""
	case len(rs) <= 8:
		return strings.Repeat("•", len(rs))
	default:
		return string(rs[:3]) + "…" + string(rs[len(rs)-4:])
	}
}

// ---------------------------------------------------------------------------
// views
// ---------------------------------------------------------------------------

// modelsView renders the current stage inside the overlay.
func (m *model) modelsView() string {
	switch m.modelsStage {
	case modelsStageKey:
		return m.modelsKeyView()
	case modelsStageModel:
		return m.modelsModelView()
	case modelsStageConfirm:
		return m.modelsConfirmView()
	}
	return m.modelsVendorView()
}

// modelsVendorView renders stage one: the preset vendor groups, the
// configured ones marked, the highlight following vendorIdx.
func (m *model) modelsVendorView() string {
	var rows []string
	for i, v := range m.modelsCat.Vendors {
		row := v.Name + dimStyle.Render(fmt.Sprintf(" · %s · %d 个模型", v.Protocol, len(v.Models)))
		if m.modelsConfigured(v.ID) {
			row += dimStyle.Render(" · 已配置")
		}
		if i == m.vendorIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · Enter 配置 Key · Esc 取消"))
	return m.overlayView("模型管理 · 选择厂商", overlayList(rows))
}

// modelsKeyView renders stage two: vendor/endpoint header, the masked
// key input, and the reuse hint when a key is already saved.
func (m *model) modelsKeyView() string {
	v := m.modelsVendor()
	var rows []string
	if v != nil {
		head := "  " + v.Name + dimStyle.Render(" · "+v.Protocol)
		if v.BaseURL != "" {
			head += dimStyle.Render(" · " + v.BaseURL)
		}
		rows = append(rows, dimStyle.Render(head))
	}
	disp := maskKey(m.keyBuf)
	if disp == "" {
		hint := "sk-..."
		if v != nil && v.KeyHint != "" {
			hint = v.KeyHint
		}
		disp = dimStyle.Render(hint)
	}
	rows = append(rows, "  API Key: "+disp+dimStyle.Render("▏"))
	if m.keyBuf == "" && m.keyExisting != "" {
		rows = append(rows, dimStyle.Render("  已保存 Key: "+maskKey(m.keyExisting)+" · 直接 Enter 保留,输入则替换"))
	}
	rows = append(rows, dimStyle.Render("  Enter 下一步(选择模型) · Esc 返回 · 留空则使用环境变量"))
	return m.overlayView("配置 API Key", overlayList(rows))
}

// modelsModelView renders stage three: the vendor's preset models with
// their parameters (context / output cap / reasoning), configured ones
// marked.
func (m *model) modelsModelView() string {
	v := m.modelsVendor()
	if v == nil {
		return m.overlayView("选择模型", "")
	}
	rows := []string{dimStyle.Render("  " + v.Name + dimStyle.Render(" · "+v.Protocol))}
	for i, pm := range v.Models {
		row := pm.DisplayName() + dimStyle.Render(" · "+presetSpec(&pm))
		if pm.Reasoning {
			row += dimStyle.Render(" · 推理")
		}
		if m.modelsModelConfigured(v.ID, pm.ID) {
			row += dimStyle.Render(" · 已配置")
		}
		if i == m.modelsIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · Enter 确认信息 · Esc 返回"))
	return m.overlayView("选择模型", overlayList(rows))
}

// modelsConfirmView renders stage four: the full summary of what is
// about to land in settings, then the reasoning-strength rows (the
// model's own labels; the preset default marked) for reasoning models.
func (m *model) modelsConfirmView() string {
	v := m.modelsVendor()
	if v == nil || m.modelsIdx < 0 || m.modelsIdx >= len(v.Models) {
		return m.overlayView("确认配置", "")
	}
	pm := v.Models[m.modelsIdx]
	key := strings.TrimSpace(m.keyBuf)
	if key == "" {
		key = m.keyExisting
	}
	keyState := "未填写(使用环境变量)"
	if key != "" {
		keyState = maskKey(key)
	}
	rows := []string{
		"  厂商:   " + v.Name,
		"  模型:   " + pm.DisplayName() + dimStyle.Render(" ("+pm.ID+")"),
		"  协议:   " + v.Protocol,
		"  参数:   " + dimStyle.Render(presetSpec(&pm)),
		"  Key:    " + keyState,
		"  写入:   " + dimStyle.Render("settings.json providers."+config.ProfileName(v.ID, pm.ID)),
	}
	choices := m.modelsEffortChoices()
	if pm.Reasoning && len(choices) > 0 {
		for i, e := range choices {
			row := e.Label + dimStyle.Render(" ("+e.Level+")")
			if e.Level == pm.DefaultEffort {
				row += dimStyle.Render(" · 预设")
			}
			if i == m.confirmIdx {
				rows = append(rows, userStyle.Render("> ")+row)
			} else {
				rows = append(rows, "  "+row)
			}
		}
		rows = append(rows, dimStyle.Render("  ↑/↓ 选择推理强度 · Enter 完成配置 · Esc 返回"))
	} else {
		rows = append(rows, dimStyle.Render("  Enter 完成配置 · Esc 返回"))
	}
	return m.overlayView("确认配置", overlayList(rows))
}

// presetSpec formats the preset parameters line (context / output cap).
func presetSpec(pm *config.PresetModel) string {
	return "上下文 " + presetTokens(pm.ContextWindow) + " · 最大输出 " + presetTokens(pm.MaxTokens)
}

// presetTokens compacts a token count (200000 → 200K, 1000000 → 1M).
func presetTokens(n int) string {
	switch {
	case n <= 0:
		return "-"
	case n%1000000 == 0:
		return fmt.Sprintf("%dM", n/1000000)
	case n%1000 == 0:
		return fmt.Sprintf("%dK", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
