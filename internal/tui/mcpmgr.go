package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"scode/internal/cli"
	"scode/internal/mcp"
)

// The /mcp overlay: list the configured MCP servers with live status;
// add, edit, toggle, and delete them. Every mutation is effective
// immediately — tools register and unregister as transcript deltas, so
// the model can call a freshly added server's tools on the next
// request (no restart; safe even mid-run, the backend hooks are
// mutex-serialized).

// mcpStage is the overlay's two screens.
type mcpStage int

const (
	mcpStageList mcpStage = iota
	mcpStageEdit
)

// Edit-screen field kinds.
const (
	mcpFieldText   = iota // typed text (name / command / url)
	mcpFieldChoice        // Enter cycles the value
	mcpFieldAction        // Enter commits (the save row)
)

type mcpField struct {
	id, label, value string
	kind             int
}

// mcpForm is the edit screen's state: plain strings parsed on save.
// cfg carries the loaded entry so advanced fields (env, headers,
// reconnect, timeouts) survive an edit untouched.
type mcpForm struct {
	editing   string // original name; "" = a new server
	name      string
	transport string // "stdio" | "streamable-http"
	command   string // stdio: command + args, space-joined
	url       string // streamable-http
	policy    string // "ask" | "allow" | "deny" ("ask" persists as empty)
	cfg       mcp.ServerConfig
}

// fields lists the edit screen's rows in order: the name row only
// exists for a new server, and the transport decides command vs URL.
func (f *mcpForm) fields() []mcpField {
	var out []mcpField
	if f.editing == "" {
		out = append(out, mcpField{"name", "名称 [A-Za-z0-9_-]", f.name, mcpFieldText})
	}
	out = append(out, mcpField{"transport", "传输", f.transport, mcpFieldChoice})
	if f.transport == "stdio" {
		out = append(out, mcpField{"command", "命令行 (命令 + 参数)", f.command, mcpFieldText})
	} else {
		out = append(out, mcpField{"url", "URL", f.url, mcpFieldText})
	}
	out = append(out, mcpField{"policy", "defaultPolicy", f.policy, mcpFieldChoice})
	out = append(out, mcpField{"save", "保存并连接", "", mcpFieldAction})
	return out
}

// Status dots for the server list (gray spans the not-live states).
var (
	mcpDotIdle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

func (m *model) openMcpManager() {
	m.mcpServers = m.app.MCPServers()
	m.mcpOpen = true
	m.mcpStage = mcpStageList
	m.mcpListIdx = 0
	m.mcpConfirmDel = false
	m.resize()
}

// mcpRefresh re-reads the file plus live status after every mutation.
func (m *model) mcpRefresh() {
	m.mcpServers = m.app.MCPServers()
	if m.mcpListIdx >= len(m.mcpServers) {
		m.mcpListIdx = max(0, len(m.mcpServers)-1)
	}
}

// mcpEdit loads one server into the form (advanced config fields ride
// along in cfg).
func (m *model) mcpEdit(info cli.MCPServerInfo) {
	cfg := info.Config
	f := mcpForm{
		editing:   info.Name,
		name:      info.Name,
		transport: cfg.Transport,
		url:       cfg.URL,
		policy:    "ask",
		cfg:       cfg,
	}
	if cfg.DefaultPolicy != "" {
		f.policy = cfg.DefaultPolicy
	}
	if cfg.Command != "" {
		f.command = strings.TrimSpace(cfg.Command + " " + strings.Join(cfg.Args, " "))
	}
	m.mcpForm = f
	m.mcpStage = mcpStageEdit
	m.mcpEditField = 0
}

func (m *model) mcpNew() {
	m.mcpForm = mcpForm{transport: "stdio", policy: "ask"}
	m.mcpStage = mcpStageEdit
	m.mcpEditField = 0
}

// handleMcpKey owns the keyboard while the overlay is open (modal:
// everything unmatched is swallowed before the textarea sees it).
func (m model) handleMcpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if m.mcpStage == mcpStageEdit {
		return m.handleMcpEditKey(msg, k)
	}
	switch k {
	case "esc":
		m.mcpOpen = false
		m.resize()
		return m, nil
	case "up":
		if n := len(m.mcpServers); n > 0 {
			m.mcpListIdx = (m.mcpListIdx - 1 + n) % n
		}
		m.mcpConfirmDel = false
		return m, nil
	case "down":
		if n := len(m.mcpServers); n > 0 {
			m.mcpListIdx = (m.mcpListIdx + 1) % n
		}
		m.mcpConfirmDel = false
		return m, nil
	case "enter", "ctrl+m":
		if idx := m.mcpListIdx; idx < len(m.mcpServers) {
			m.mcpEdit(m.mcpServers[idx])
		}
		return m, nil
	case "n":
		m.mcpNew()
		return m, nil
	case "t":
		if idx := m.mcpListIdx; idx < len(m.mcpServers) {
			name := m.mcpServers[idx].Name
			on, err := m.app.MCPToggle(name)
			if err != nil {
				return m, m.showToast("mcp: " + err.Error())
			}
			m.mcpRefresh()
			if on {
				return m, m.showToast("mcp(" + name + ") 已启用,连接中")
			}
			return m, m.showToast("mcp(" + name + ") 已停用")
		}
		return m, nil
	case "d":
		idx := m.mcpListIdx
		if idx >= len(m.mcpServers) {
			return m, nil
		}
		name := m.mcpServers[idx].Name
		if !m.mcpConfirmDel {
			m.mcpConfirmDel = true
			return m, nil
		}
		if err := m.app.MCPRemove(name); err != nil {
			return m, m.showToast("mcp: " + err.Error())
		}
		m.mcpConfirmDel = false
		m.mcpRefresh()
		return m, m.showToast("mcp(" + name + ") 已删除")
	}
	return m, nil // modal: swallow everything else
}

// handleMcpEditKey drives the form: ↑/↓ move the field focus, Enter
// cycles choices or commits on the save row, text lands in the focused
// text field (spaces included — commands need them).
func (m model) handleMcpEditKey(msg tea.KeyPressMsg, k string) (tea.Model, tea.Cmd) {
	switch k {
	case "esc":
		m.mcpStage = mcpStageList
		m.mcpRefresh()
		return m, nil
	case "up":
		m.mcpEditField = (m.mcpEditField - 1 + len(m.mcpForm.fields())) % len(m.mcpForm.fields())
		return m, nil
	case "down":
		m.mcpEditField = (m.mcpEditField + 1) % len(m.mcpForm.fields())
		return m, nil
	case "enter", "ctrl+m":
		fields := m.mcpForm.fields()
		f := min(m.mcpEditField, len(fields)-1)
		switch fields[f].kind {
		case mcpFieldChoice:
			m.mcpCycleChoice(fields[f].id)
			return m, nil
		case mcpFieldAction:
			return m.mcpCommit()
		}
		return m, nil
	case "backspace":
		m.mcpBackspace()
		return m, nil
	case "ctrl+u":
		m.mcpClearField()
		return m, nil
	default:
		if msg.Text != "" {
			m.mcpType(msg.Text)
		}
		return m, nil
	}
}

// mcpCycleChoice rotates a choice field's value.
func (m *model) mcpCycleChoice(id string) {
	switch id {
	case "transport":
		if m.mcpForm.transport == "stdio" {
			m.mcpForm.transport = "streamable-http"
		} else {
			m.mcpForm.transport = "stdio"
		}
	case "policy":
		switch m.mcpForm.policy {
		case "ask":
			m.mcpForm.policy = "allow"
		case "allow":
			m.mcpForm.policy = "deny"
		default:
			m.mcpForm.policy = "ask"
		}
	}
}

// mcpFocusedField returns the field under the edit focus (nil-safe).
func (m *model) mcpFocusedField() *mcpField {
	fields := m.mcpForm.fields()
	if m.mcpEditField < 0 || m.mcpEditField >= len(fields) {
		return nil
	}
	return &fields[m.mcpEditField]
}

func (m *model) mcpType(s string) {
	f := m.mcpFocusedField()
	if f == nil || f.kind != mcpFieldText {
		return
	}
	switch f.id {
	case "name":
		m.mcpForm.name += s
	case "command":
		m.mcpForm.command += s
	case "url":
		m.mcpForm.url += s
	}
}

func (m *model) mcpBackspace() {
	f := m.mcpFocusedField()
	if f == nil || f.kind != mcpFieldText {
		return
	}
	drop := func(s string) string {
		if rs := []rune(s); len(rs) > 0 {
			return string(rs[:len(rs)-1])
		}
		return s
	}
	switch f.id {
	case "name":
		m.mcpForm.name = drop(m.mcpForm.name)
	case "command":
		m.mcpForm.command = drop(m.mcpForm.command)
	case "url":
		m.mcpForm.url = drop(m.mcpForm.url)
	}
}

func (m *model) mcpClearField() {
	f := m.mcpFocusedField()
	if f == nil || f.kind != mcpFieldText {
		return
	}
	switch f.id {
	case "name":
		m.mcpForm.name = ""
	case "command":
		m.mcpForm.command = ""
	case "url":
		m.mcpForm.url = ""
	}
}

// mcpCommit parses the form into a ServerConfig, persists it, and
// applies it live (Add or Restart), then closes the overlay. The
// supervisor connects asynchronously; its tools register as a
// transcript delta the moment the sync lands.
func (m model) mcpCommit() (tea.Model, tea.Cmd) {
	f := m.mcpForm
	cfg := f.cfg
	cfg.Transport = f.transport
	cfg.Command, cfg.Args, cfg.URL = "", nil, ""
	if f.transport == "stdio" {
		if parts := strings.Fields(f.command); len(parts) > 0 {
			cfg.Command = parts[0]
			cfg.Args = parts[1:]
		}
	} else {
		cfg.URL = strings.TrimSpace(f.url)
	}
	cfg.DefaultPolicy = ""
	if f.policy != "ask" {
		cfg.DefaultPolicy = f.policy
	}
	name := strings.TrimSpace(f.name)
	var err error
	if f.editing == "" {
		err = m.app.MCPAdd(name, cfg)
	} else {
		err = m.app.MCPRestart(f.editing, cfg)
	}
	m.mcpOpen = false
	m.resize()
	if err != nil {
		return m, m.showToast("mcp: " + err.Error())
	}
	if f.editing == "" {
		return m, m.showToast("mcp(" + name + ") 已保存,连接中")
	}
	return m, m.showToast("mcp(" + f.editing + ") 已重启")
}

// mcpView renders the open overlay (list or edit screen).
func (m *model) mcpView() string {
	if m.mcpStage == mcpStageEdit {
		return m.mcpEditView()
	}
	return m.mcpListView()
}

// mcpStatusDot maps live status to a colored dot + human state text.
func mcpStatusDot(s cli.MCPServerInfo) (lipgloss.Style, string) {
	if !s.Config.IsEnabled() {
		return mcpDotIdle, "已停用"
	}
	if s.Status == nil {
		return mcpDotIdle, "未运行"
	}
	switch s.Status.State {
	case mcp.StatusConnected:
		return dotOK, fmt.Sprintf("已连接 · %d 工具", s.Status.Tools)
	case mcp.StatusReconnecting:
		return dotSandbox, fmt.Sprintf("重连中 %d/%d", s.Status.Attempts, s.Status.MaxAttempts)
	case mcp.StatusGaveUp:
		return dotFail, "已放弃(重连耗尽)"
	default:
		return mcpDotIdle, "连接中…"
	}
}

func (m *model) mcpListView() string {
	var rows []string
	for i, s := range m.mcpServers {
		dot, state := mcpStatusDot(s)
		row := fmt.Sprintf("%s %-18s %-14s %s", dot.Render("●"), s.Name, s.Config.Transport, state)
		if i == m.mcpListIdx {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	if len(rows) == 0 {
		rows = append(rows, dimStyle.Render("  (无服务器 — 按 n 新建)"))
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 选择 · enter 编辑 · n 新建 · t 启停 · d 删除 · esc 关闭"))
	if m.mcpConfirmDel && m.mcpListIdx < len(m.mcpServers) {
		rows = append(rows, warnStyle.Render("  再按 d 确认删除 "+m.mcpServers[m.mcpListIdx].Name))
	}
	return m.overlayView("MCP 服务器", overlayList(rows))
}

func (m *model) mcpEditView() string {
	f := m.mcpForm
	title := "MCP 新建"
	if f.editing != "" {
		title = "MCP 编辑: " + f.editing
	}
	var rows []string
	for i, field := range f.fields() {
		row := field.label
		if field.value != "" {
			row += ": " + field.value
		}
		if field.kind == mcpFieldChoice {
			row += dimStyle.Render("  (enter 切换)")
		}
		if i == m.mcpEditField {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	rows = append(rows, dimStyle.Render("  ↑/↓ 字段 · enter 切换选项/保存 · 直接输入文字 · ctrl+u 清空 · esc 返回"))
	return m.overlayView(title, overlayList(rows))
}
