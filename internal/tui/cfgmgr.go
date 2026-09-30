package tui

// /config: the basic-configuration panel — a checklist over the
// settings the harness exposes. Toggles persist immediately and take
// effect at once where the behavior is live (auto-compact re-parks the
// threshold; log retention prunes on the spot). The memory/checkpoint
// switches are the durable config their subsystems read.

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// cfgRetentionPresets are the retention choices (days; 0 = never).
var cfgRetentionPresets = []int{0, 7, 14, 30, 90}

type cfgItem struct {
	id    string
	label string
	desc  string
}

// cfgItems is the panel's row order; the description doubles as the
// bottom explanation line for the highlighted row.
var cfgItems = []cfgItem{
	{id: "autoCompact", label: "上下文自动压缩", desc: "接近模型窗口时自动总结压缩历史上下文(即时生效)"},
	{id: "retention", label: "日志清理周期", desc: "启动时删除超过该天数的会话日志文件(0 = 永不清理)"},
	{id: "autoMemory", label: "Auto Memory · 自动记忆", desc: "AI 自动记住对话中的关键信息(配置已持久化,记忆子系统读取)"},
	{id: "typedMemory", label: "Typed Memory · 分类记忆", desc: "记忆按分类存储(配置已持久化,记忆子系统读取)"},
	{id: "memoryRelevance", label: "Memory Relevance · 相关性选择", desc: "关闭 = 由 AI 自动筛选相关记忆,不手动选择(配置已持久化)"},
	{id: "memoryAutoExtract", label: "Memory Auto Extraction · 自动提取", desc: "关闭 = 会话结束不自动抽取记忆,需手动触发(配置已持久化)"},
	{id: "rewind", label: "Rewind code · 检查点回滚", desc: "允许把代码回滚到之前的 AI 检查点(配置已持久化,检查点子系统读取)"},
	{id: "clipWatch", label: "剪贴板图片读取 (alt+v / ctrl+v)", desc: "alt+v / ctrl+v 探测一次剪贴板(ctrl+v 常被 Windows 终端拦截,alt+v 更可靠);未读到图片则循环监控约 15 秒,检测到图片自动附加后停止,再次触发重新激活。空闲时零调用,担心杀毒告警可关闭"},
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
		m.cfgIdx = (m.cfgIdx - 1 + len(cfgItems)) % len(cfgItems)
		return m, nil
	case "down":
		m.cfgIdx = (m.cfgIdx + 1) % len(cfgItems)
		return m, nil
	case "enter", "ctrl+m", "t", " ":
		return m.cfgActivate()
	}
	return m, nil // modal: swallow everything else
}

// cfgActivate toggles the highlighted row (retention cycles its
// presets), persisting immediately.
func (m model) cfgActivate() (tea.Model, tea.Cmd) {
	item := cfgItems[m.cfgIdx]
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
	value := func(id string) string {
		onOff := func(b bool) string {
			if b {
				return "开"
			}
			return "关"
		}
		switch id {
		case "autoCompact":
			return onOff(c.AutoCompact)
		case "retention":
			if c.LogRetentionDays <= 0 {
				return "永不清理"
			}
			return strconv.Itoa(c.LogRetentionDays) + " 天"
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
		return ""
	}
	labelW := 0
	for _, item := range cfgItems {
		if w := lipgloss.Width(item.label); w > labelW {
			labelW = w
		}
	}
	rows := make([]string, 0, len(cfgItems)+2)
	for i, item := range cfgItems {
		val := value(item.id)
		v := userStyle.Render(val)
		if val == "关" {
			v = dimStyle.Render(val)
		}
		row := item.label + strings.Repeat(" ", labelW-lipgloss.Width(item.label)+3) + v
		if i == m.cfgIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · enter/t 切换 · esc 关闭"))
	rows = append(rows, dimStyle.Render("  "+truncate(cfgItems[m.cfgIdx].desc, max(20, m.width-8))))
	return m.overlayView("基本配置", overlayList(rows))
}
