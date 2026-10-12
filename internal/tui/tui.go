// Package tui renders watcher snapshots with bubbletea.
package tui

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/frankie0736/codex-watchdog/internal/detector"
	"github.com/frankie0736/codex-watchdog/internal/limiter"
	"github.com/frankie0736/codex-watchdog/internal/watcher"
)

type view int

const (
	viewList view = iota
	viewDetail
	viewLog
	viewHelp
)

type snapshotMsg watcher.Snapshot
type tickMsg time.Time

type Model struct {
	w       *watcher.Watcher
	snap    watcher.Snapshot
	hasSnap bool
	rows    []watcher.Row // rows currently in the table
	table   table.Model
	view    view
	showAll bool
	confirm bool // waiting for "y" to enable live sending
	flash   string
	width   int
	height  int
	logPos  int
}

func New(w *watcher.Watcher) Model {
	t := table.New(table.WithFocused(true))
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).BorderStyle(lipgloss.NormalBorder()).BorderBottom(true)
	st.Selected = st.Selected.Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Bold(false)
	t.SetStyles(st)
	return Model{w: w, table: t, width: 120, height: 30}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(waitSnapshot(m.w), tick())
}

func waitSnapshot(w *watcher.Watcher) tea.Cmd {
	return func() tea.Msg { return snapshotMsg(<-w.Updates()) }
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.rebuild()
		return m, nil
	case snapshotMsg:
		m.snap, m.hasSnap = watcher.Snapshot(msg), true
		m.rebuild()
		return m, waitSnapshot(m.w)
	case tickMsg:
		return m, tick() // re-render countdowns
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if m.confirm {
		m.confirm = false
		if s == "y" || s == "Y" {
			m.w.SetDryRun(false)
			m.flash = "LIVE sending enabled"
		} else {
			m.flash = "kept DRY-RUN"
		}
		return m, nil
	}
	switch s {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "esc":
		m.view = viewList
		return m, nil
	case "?":
		m.view = viewHelp
		return m, nil
	case "p":
		m.w.TogglePause()
		return m, nil
	case "r":
		m.w.Refresh()
		m.flash = "refreshing…"
		return m, nil
	case "d":
		if m.w.DryRun() {
			m.confirm = true
		} else {
			m.w.SetDryRun(true)
			m.flash = "DRY-RUN enabled"
		}
		return m, nil
	case "a":
		m.showAll = !m.showAll
		m.rebuild()
		return m, nil
	case "l":
		m.view, m.logPos = viewLog, 0
		return m, nil
	case "enter":
		if m.view == viewList && len(m.rows) > 0 {
			m.view = viewDetail
		}
		return m, nil
	case "c":
		if r, ok := m.selected(); ok {
			if err := copyText(r.ThreadID); err != nil {
				m.flash = "copy failed: " + err.Error()
			} else {
				m.flash = "copied " + r.ThreadID
			}
		}
		return m, nil
	}
	if m.view == viewLog {
		switch s {
		case "j", "down":
			m.logPos = min(m.logPos+1, max(0, len(m.snap.Recent)-1))
		case "k", "up":
			m.logPos = max(0, m.logPos-1)
		}
		return m, nil
	}
	if m.view == viewList {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(k)
		return m, cmd
	}
	return m, nil
}

func (m Model) selected() (watcher.Row, bool) {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.rows) {
		return watcher.Row{}, false
	}
	return m.rows[i], true
}

func visibleByDefault(r watcher.Row) bool {
	switch r.Class {
	case detector.Idle, detector.Orphaned, detector.Stale:
		return r.Status == watcher.StatusRecovered
	}
	return true
}

func (m *Model) rebuild() {
	var selID string
	if r, ok := m.selected(); ok {
		selID = r.HomePath + "|" + r.ThreadID
	}
	m.rows = m.rows[:0]
	for _, r := range m.snap.Rows {
		if m.showAll || visibleByDefault(r) {
			m.rows = append(m.rows, r)
		}
	}

	fixed := 16 + 14 + 12 + 10 + 8 + 5 + 7*2
	flex := max(20, m.width-fixed-4)
	titleW := flex * 2 / 5
	reasonW := flex - titleW
	m.table.SetColumns([]table.Column{
		{Title: "状态", Width: 16},
		{Title: "来源", Width: 14},
		{Title: "Home", Width: 12},
		{Title: "Thread", Width: 10},
		{Title: "标题", Width: titleW},
		{Title: "最近活动", Width: 8},
		{Title: "原因 / 错误", Width: reasonW},
		{Title: "发送", Width: 5},
	})
	now := time.Now()
	trs := make([]table.Row, len(m.rows))
	cursor := 0
	for i, r := range m.rows {
		if r.HomePath+"|"+r.ThreadID == selID {
			cursor = i
		}
		trs[i] = table.Row{
			r.Status,
			r.Source,
			r.HomeLabel,
			short(r.ThreadID, 8),
			oneLine(r.Title),
			ago(now, r.LastActivity),
			oneLine(reasonText(r, now)),
			fmt.Sprint(r.Attempts),
		}
	}
	m.table.SetRows(trs)
	m.table.SetHeight(max(3, m.height-8))
	m.table.SetWidth(m.width)
	if cursor < len(trs) {
		m.table.SetCursor(cursor)
	}
}

func reasonText(r watcher.Row, now time.Time) string {
	s := r.Reason
	if !r.Until.IsZero() && r.Until.After(now) {
		s += fmt.Sprintf(" (%s)", r.Until.Sub(now).Round(time.Second))
	}
	if s == "" {
		s = r.Error
	} else if r.Error != "" && r.Class != detector.Capacity {
		s += " · " + r.Error
	}
	return s
}

var (
	bold   = lipgloss.NewStyle().Bold(true)
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	boxed  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

func (m Model) View() string {
	if !m.hasSnap {
		return "codex-watchdog: loading…\n"
	}
	header := m.header()
	var body string
	switch m.view {
	case viewDetail:
		body = m.detail()
	case viewLog:
		body = m.logView()
	case viewHelp:
		body = help()
	default:
		body = m.table.View()
	}
	footer := dim.Render("q 退出  p 暂停  d dry-run  r 刷新  a 全部/活跃  enter 详情  c 复制ID  l 日志  ? 帮助  esc 返回")
	if m.confirm {
		footer = red.Render("启用真实发送（会执行 codex queue）？按 y 确认，其它键取消")
	} else if m.flash != "" {
		footer = m.flash + "   " + footer
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m Model) header() string {
	s := m.snap
	mode := green.Render("AUTO ON")
	if s.DryRun {
		mode = yellow.Render("DRY-RUN")
	}
	if s.Paused {
		mode = red.Render("PAUSED") + " " + mode
	}
	active := s.Count(func(r watcher.Row) bool { return r.Class == detector.Active })
	capacity := s.Count(func(r watcher.Row) bool { return r.Class == detector.Capacity })
	waiting := s.Count(func(r watcher.Row) bool {
		switch limiter.Status(r.Status) {
		case limiter.WaitGrace, limiter.Cooldown, limiter.RateLimited, limiter.Backlog:
			return true
		}
		return false
	})
	attention := s.Count(func(r watcher.Row) bool { return r.Status == string(limiter.NeedsAttention) })

	var homes []string
	for _, h := range s.Homes {
		if h.Err != "" {
			homes = append(homes, red.Render(h.Label+" ✗"))
		} else {
			homes = append(homes, fmt.Sprintf("%s(%d)", h.Label, h.Threads))
		}
	}
	line1 := fmt.Sprintf("%s  %s  homes: %s  run %s", bold.Render("codex-watchdog"), mode, strings.Join(homes, " "), s.RunID)
	line2 := fmt.Sprintf("活跃 %d  容量错误 %d  等待限流 %d  需人工 %d  本次已发送 %d  轮询 %s  消息 %q",
		active, capacity, waiting, attention, s.SentThisRun, s.At.Format("15:04:05"), s.Config.Message)
	if attention > 0 {
		line2 = strings.Replace(line2, fmt.Sprintf("需人工 %d", attention), red.Render(fmt.Sprintf("需人工 %d", attention)), 1)
	}
	lines := []string{line1, line2}
	if s.Err != "" {
		lines = append(lines, red.Render("error: "+s.Err))
	}
	for _, h := range s.Homes {
		if h.Err != "" {
			lines = append(lines, red.Render(h.Label+": "+h.Err))
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) detail() string {
	r, ok := m.selected()
	if !ok {
		return "no selection"
	}
	now := time.Now()
	kv := [][2]string{
		{"Thread", r.ThreadID},
		{"标题", r.Title},
		{"状态", r.Status + "  (" + string(r.Class) + ")"},
		{"原因", reasonText(r, now)},
		{"来源", r.Source},
		{"Codex home", r.HomePath},
		{"模型", r.Model},
		{"工作目录", r.Cwd},
		{"最新 turn", r.TurnID + "  " + r.TurnStatus},
		{"最近活动", r.LastActivity.Format("2006-01-02 15:04:05") + "  (" + ago(now, r.LastActivity) + " ago)"},
		{"错误", r.Error},
		{"会话已打开", fmt.Sprint(r.Attached)},
		{"codex 队列积压", fmt.Sprint(r.PendingQueue)},
		{"发送次数", fmt.Sprint(r.Attempts)},
	}
	if a := r.LastAttempt; a != nil {
		kv = append(kv,
			[2]string{"最近发送", fmt.Sprintf("#%d %s %s dry_run=%v", a.No, a.Result, a.At().Format("15:04:05"), a.DryRun)},
			[2]string{"发送详情", a.Detail},
			[2]string{"发送 turn", a.TurnID},
		)
	}
	valueW := max(10, m.width-4-4-15) // box border+padding, label column
	var b strings.Builder
	for _, p := range kv {
		label := p[0] + strings.Repeat(" ", max(0, 14-lipgloss.Width(p[0])))
		fmt.Fprintf(&b, "%s %s\n", bold.Render(label), ansi.Truncate(oneLine(p[1]), valueW, "…"))
	}
	return boxed.Width(max(40, m.width-4)).Render(strings.TrimRight(b.String(), "\n"))
}

func (m Model) logView() string {
	recent := m.snap.Recent
	if len(recent) == 0 {
		return boxed.Render("暂无自动处理记录")
	}
	h := max(3, m.height-8)
	end := min(len(recent), m.logPos+h)
	var b strings.Builder
	for _, a := range recent[m.logPos:end] {
		mode := "LIVE"
		if a.DryRun {
			mode = "DRY "
		}
		line := fmt.Sprintf("%s %s %-11s %s #%d %s %s",
			a.DecidedAt.Format("01-02 15:04:05"), mode, a.Result, short(a.ThreadID, 8), a.No,
			short(a.TurnID, 8), oneLine(a.Detail))
		fmt.Fprintln(&b, ansi.Truncate(line, max(10, m.width-8), "…"))
	}
	return boxed.Width(max(40, m.width-4)).Render(strings.TrimRight(b.String(), "\n"))
}

func help() string {
	return boxed.Render(`状态说明
  ACTIVE           最新 turn 运行中（在 active_max_age 内）
  ORPHANED         inProgress 但太旧，进程大概率已退出（按 a 显示）
  WAIT_GRACE       容量错误，等待 grace period
  COOLDOWN         同一会话冷却中
  RATE_LIMITED     全局每分钟上限
  BACKLOG          codex 队列里还有未消费消息，不再叠加
  QUEUEING         codex queue 执行中
  QUEUED           已入队，等待新 turn 出现
  QUEUE_FAILED     执行失败，等待重试
  NEEDS_ATTENTION  已达最大尝试次数，需人工处理
  DRY_RUN          dry-run 模式下记录为“本应发送”
  RECOVERED        发送后出现了新 turn
  EXCLUDED         容量错误，但来源/归档状态不允许自动处理
  DETACHED         容量错误，但会话已退出（没有 codex 进程加载它），不发送
  SKIPPED          启动前已有的错误且配置为不处理`)
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func copyText(s string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}
