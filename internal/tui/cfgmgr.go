package tui

// /config: the basic-configuration panel — a checklist over the
// settings the harness exposes. Toggles persist immediately and take
// effect at once where the behavior is live (auto-compact re-parks the
// threshold; log retention prunes on the spot). The memory/checkpoint
// switches are the durable config their subsystems read.

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"scode/internal/i18n"
)

// cfgRetentionPresets are the retention choices (days; 0 = never).
var cfgRetentionPresets = []int{0, 7, 14, 30, 90}

type cfgItem struct {
	id    string
	label string
	desc  string
}

// cfgItems is the panel's row order; the description doubles as the
// bottom explanation line for the highlighted row. A function (not a
// var) so the display language is read at render time.
func cfgItems() []cfgItem {
	return []cfgItem{
		{id: "autoCompact", label: i18n.T("tui.config.autoCompact.label"), desc: i18n.T("tui.config.autoCompact.desc")},
		{id: "retention", label: i18n.T("tui.config.retention.label"), desc: i18n.T("tui.config.retention.desc")},
		{id: "autoMemory", label: i18n.T("tui.config.autoMemory.label"), desc: i18n.T("tui.config.autoMemory.desc")},
		{id: "typedMemory", label: i18n.T("tui.config.typedMemory.label"), desc: i18n.T("tui.config.typedMemory.desc")},
		{id: "memoryRelevance", label: i18n.T("tui.config.memoryRelevance.label"), desc: i18n.T("tui.config.memoryRelevance.desc")},
		{id: "memoryAutoExtract", label: i18n.T("tui.config.memoryAutoExtract.label"), desc: i18n.T("tui.config.memoryAutoExtract.desc")},
		{id: "rewind", label: i18n.T("tui.config.rewind.label"), desc: i18n.T("tui.config.rewind.desc")},
		{id: "clipWatch", label: i18n.T("tui.config.clipWatch.label"), desc: i18n.T("tui.config.clipWatch.desc")},
	}
}

func (m *model) openConfigPanel() {
	m.cfgOpen = true
	m.cfgIdx = 0
	m.cfgSnapshot = m.app.TUIConfig()
	m.resize()
}

func (m model) handleConfigKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.cfgOpen = false
		m.resize()
		return m, nil
	case "up":
		m.cfgIdx = (m.cfgIdx - 1 + len(cfgItems())) % len(cfgItems())
		return m, nil
	case "down":
		m.cfgIdx = (m.cfgIdx + 1) % len(cfgItems())
		return m, nil
	case "enter", "ctrl+m", "t", " ":
		return m.cfgActivate()
	}
	return m, nil // modal: swallow everything else
}

// cfgActivate toggles the highlighted row (retention cycles its
// presets), persisting immediately.
func (m model) cfgActivate() (tea.Model, tea.Cmd) {
	item := cfgItems()[m.cfgIdx]
	var err error
	switch item.id {
	case "autoCompact":
		err = m.app.SetAutoCompact(!m.cfgSnapshot.AutoCompact)
	case "retention":
		err = m.app.SetLogRetention(cfgNextRetention(m.cfgSnapshot.LogRetentionDays))
	case "autoMemory":
		err = m.app.SetConfigBool("autoMemory", !m.cfgSnapshot.AutoMemory)
	case "typedMemory":
		err = m.app.SetConfigBool("typedMemory", !m.cfgSnapshot.TypedMemory)
	case "memoryRelevance":
		err = m.app.SetConfigBool("memoryRelevance", !m.cfgSnapshot.MemoryRelevance)
	case "memoryAutoExtract":
		err = m.app.SetConfigBool("memoryAutoExtraction", !m.cfgSnapshot.MemoryAutoExtract)
	case "rewind":
		err = m.app.SetConfigBool("rewindCheckpoints", !m.cfgSnapshot.RewindCheckpoints)
	case "clipWatch":
		next := !m.cfgSnapshot.ClipboardWatch
		if err = m.app.SetConfigBool("clipboardWatch", next); err == nil {
			// Live effect: the ctrl+v branch consults this flag.
			m.clipWatch = next
			m.cfgSnapshot = m.app.TUIConfig()
		}
	}
	if err != nil {
		return m, m.showToast("config: " + err.Error())
	}
	m.cfgSnapshot = m.app.TUIConfig()
	return m, nil
}

// cfgNextRetention walks the preset cycle, wrapping unknown values
// (hand-edited settings) to the first preset.
func cfgNextRetention(cur int) int {
	for i, d := range cfgRetentionPresets {
		if d == cur {
			return cfgRetentionPresets[(i+1)%len(cfgRetentionPresets)]
		}
	}
	return cfgRetentionPresets[1%len(cfgRetentionPresets)]
}

func (m *model) configView() string {
	c := m.cfgSnapshot
	// value renders a row's current setting; off=true dims the cell
	// (previously the code compared against the literal "关", which
	// coupling the display language would break).
	value := func(id string) (text string, off bool) {
		onOff := func(b bool) (string, bool) {
			if b {
				return i18n.T("tui.config.on"), false
			}
			return i18n.T("tui.config.off"), true
		}
		switch id {
		case "autoCompact":
			return onOff(c.AutoCompact)
		case "retention":
			if c.LogRetentionDays <= 0 {
				return i18n.T("tui.config.never"), false
			}
			return i18n.Tf("tui.config.days", c.LogRetentionDays), false
		case "autoMemory":
			return onOff(c.AutoMemory)
		case "typedMemory":
			return onOff(c.TypedMemory)
		case "memoryRelevance":
			return onOff(c.MemoryRelevance)
		case "memoryAutoExtract":
			return onOff(c.MemoryAutoExtract)
		case "rewind":
			return onOff(c.RewindCheckpoints)
		case "clipWatch":
			return onOff(c.ClipboardWatch)
		}
		return "", false
	}
	items := cfgItems()
	labelW := 0
	for _, item := range items {
		if w := lipgloss.Width(item.label); w > labelW {
			labelW = w
		}
	}
	rows := make([]string, 0, len(items)+2)
	for i, item := range items {
		val, off := value(item.id)
		v := userStyle.Render(val)
		if off {
			v = dimStyle.Render(val)
		}
		row := item.label + strings.Repeat(" ", labelW-lipgloss.Width(item.label)+3) + v
		if i == m.cfgIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render(i18n.T("tui.config.hint")))
	rows = append(rows, dimStyle.Render("  "+truncate(items[m.cfgIdx].desc, max(20, m.width-8))))
	return m.overlayView(i18n.T("tui.config.title"), overlayList(rows))
}
