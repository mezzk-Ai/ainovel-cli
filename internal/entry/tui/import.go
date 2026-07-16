package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/host/imp"
)

// importState 是 /import 命令运行期间的模态状态。
//
// 模态在导入开始时创建，跟随事件流推进；完成或出错后保留在屏上等用户 Esc 关闭。
// Esc 在运行中触发取消（ctx.Cancel），交由 runner 在下一事件点收尾。
type importState struct {
	reqID      int
	source     string
	stage      imp.Stage
	current    int
	total      int
	startedAt  time.Time
	finishedAt time.Time
	history    []importLine
	err        error
	done       bool // 终态（完成/出错）
	paused     bool // 管线在 awaiting 处停下、事件通道已关闭：面板可关闭，非终态
	cancel     context.CancelFunc
	viewport   viewport.Model
}

type importLine struct {
	at      time.Time
	stage   imp.Stage
	current int
	total   int
	message string
	err     error
}

func newImportState(reqID int, source string, width, height int, cancel context.CancelFunc) *importState {
	boxW, boxH := reportModalSize(width, height)
	contentW := paddedModalContentWidth(boxW)
	vp := viewport.New(contentW, boxH-4)
	s := &importState{
		reqID:     reqID,
		source:    source,
		startedAt: time.Now(),
		stage:     imp.StageIngesting,
		cancel:    cancel,
		viewport:  vp,
	}
	s.refresh(contentW)
	return s
}

func (s *importState) appendEvent(ev imp.Event, contentW int) {
	s.stage = ev.Stage
	s.current = ev.Current
	s.total = ev.Total
	if ev.Err != nil {
		s.err = ev.Err
	}
	s.history = append(s.history, importLine{
		at: ev.Time, stage: ev.Stage, current: ev.Current, total: ev.Total,
		message: ev.Message, err: ev.Err,
	})
	if ev.Stage == imp.StageDone || ev.Stage == imp.StageError {
		s.done = true
		s.finishedAt = ev.Time
	}
	s.refresh(contentW)
}

func (s *importState) refresh(contentW int) {
	titleStyle := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(colorDim)
	mutedStyle := lipgloss.NewStyle().Foreground(colorMuted)
	okStyle := lipgloss.NewStyle().Foreground(colorSuccess)
	errStyle := lipgloss.NewStyle().Foreground(colorError)
	stageStyle := lipgloss.NewStyle().Foreground(colorAccent2)

	var b strings.Builder
	b.WriteString(titleStyle.Render("导入外部小说"))
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("源文件 "))
	b.WriteString(s.source)
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("开始 "))
	b.WriteString(formatReportTime(s.startedAt))
	if !s.finishedAt.IsZero() {
		b.WriteString(dimStyle.Render("  完成 "))
		b.WriteString(formatReportTime(s.finishedAt))
	}
	b.WriteString("\n\n")

	// 当前阶段行
	b.WriteString(mutedStyle.Render("阶段 "))
	b.WriteString(stageStyle.Render(string(s.stage)))
	if s.total > 0 {
		b.WriteString(mutedStyle.Render("  进度 "))
		if s.current > 0 {
			b.WriteString(fmt.Sprintf("%d/%d", s.current, s.total))
		} else {
			b.WriteString(fmt.Sprintf("0/%d", s.total))
		}
	}
	b.WriteString("\n\n")

	// 历史日志
	b.WriteString(titleStyle.Render("流程日志"))
	b.WriteString(" ")
	b.WriteString(dimStyle.Render(fmt.Sprintf("(%d 条)", len(s.history))))
	b.WriteString("\n")
	for _, ln := range s.history {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(ln.at.Format("15:04:05")))
		b.WriteString(" ")
		b.WriteString(stageStyle.Render(string(ln.stage)))
		if ln.total > 0 && ln.current > 0 {
			b.WriteString(mutedStyle.Render(fmt.Sprintf(" %d/%d", ln.current, ln.total)))
		}
		b.WriteString(" ")
		if ln.err != nil {
			b.WriteString(errStyle.Render(ln.message + " — " + ln.err.Error()))
		} else {
			b.WriteString(wrapText(ln.message, contentW))
		}
	}

	// 收尾提示
	b.WriteString("\n\n")
	switch {
	case s.err != nil:
		b.WriteString(errStyle.Render("导入失败"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("Esc 关闭面板"))
	case s.paused && s.stage == imp.StageAwaitingConfirmation:
		b.WriteString(okStyle.Render("切分完成，等待你核对"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("y 确认切分并继续；需调整切分可 Esc 后用 /import --guide=<自然语言说明>；Esc 关闭面板"))
	case s.paused:
		// 管线在等待裁定处停下，通道已关闭：按面板内提示操作后 Esc 关闭。
		b.WriteString(okStyle.Render("导入已暂停，等待你的操作"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("按上方提示继续（如 /import --story=open|closed）；Esc 关闭面板"))
	case s.done:
		b.WriteString(okStyle.Render("导入完成，Foundation 与章节已就绪"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("Esc 关闭面板；如需续写请在主界面按正常门禁继续"))
	default:
		b.WriteString(dimStyle.Render("Esc 取消导入"))
	}

	s.viewport.SetContent(b.String())
	if !s.done && !s.paused {
		s.viewport.GotoBottom()
	}
}

func renderImportModal(width, height int, s *importState) string {
	if s == nil {
		return ""
	}
	boxW, boxH := reportModalSize(width, height)
	contentW := paddedModalContentWidth(boxW)
	if s.viewport.Width != contentW {
		s.viewport.Width = contentW
		s.refresh(contentW)
	}
	if s.viewport.Height != boxH-4 {
		s.viewport.Height = boxH - 4
	}

	hint := "  ↑↓ 滚动 · Esc 取消/关闭"
	if s.paused && s.stage == imp.StageAwaitingConfirmation {
		hint = "  ↑↓ 滚动 · y 确认切分 · Esc 关闭"
	}
	modal := renderPaddedModalFrame(boxW, boxH, "外部小说导入", hint,
		strings.Split(s.viewport.View(), "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal)
}

func (m Model) handleImportKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.importer == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		// 仍在运行（未终态、未暂停）→ Esc 取消，交 runner 收尾；已终态或已在 awaiting 处停下
		// （通道关闭）→ Esc 关闭面板。缺少 paused 分支会让 awaiting 停机后面板无法关闭（卡死）。
		if !m.importer.done && !m.importer.paused && m.importer.cancel != nil {
			m.importer.cancel()
			return m, nil
		}
		m.importer = nil
		return m, m.textarea.Focus()
	case tea.KeyUp:
		m.importer.viewport.ScrollUp(1)
	case tea.KeyDown:
		m.importer.viewport.ScrollDown(1)
	case tea.KeyPgUp:
		m.importer.viewport.HalfPageUp()
	case tea.KeyPgDown:
		m.importer.viewport.HalfPageDown()
	case tea.KeyRunes:
		// 切分确认暂停处按 y = 原地重跑 /import --yes（无路径恢复），一次性放行当前切分。
		if len(msg.Runes) == 1 && (msg.Runes[0] == 'y' || msg.Runes[0] == 'Y') &&
			m.importer.paused && m.importer.stage == imp.StageAwaitingConfirmation {
			return m.confirmImportSegmentation()
		}
	}
	return m, nil
}

// confirmImportSegmentation 把"看过预览后放行"缩成一个按键：底层与用户重敲 /import --yes 完全相同
// （恢复是无状态的，管线从 confirmation 缺失处继续）。AutoConfirm 只随本次 Options 生效、
// 不写 intent.json，因此之后 --guide 重切出的新切分仍会停下核对。
// 沿用旧面板的源文件名与流程日志，让章节预览在继续分析时仍可回滚查看。
func (m Model) confirmImportSegmentation() (tea.Model, tea.Cmd) {
	prev := m.importer
	m.importSeq++
	state, listenCmd, err := startImport(m.runtime, m.importSeq, []string{"--yes"}, m.width, m.height)
	if err != nil {
		m.applyEvent(host.Event{
			Time: time.Now(), Category: "ERROR", Summary: "确认切分失败：" + err.Error(), Level: "error",
		})
		return m, nil
	}
	state.source = prev.source
	state.history = append([]importLine(nil), prev.history...)
	boxW, _ := reportModalSize(m.width, m.height)
	state.refresh(paddedModalContentWidth(boxW))
	m.importer = state
	return m, listenCmd
}

// importEventMsg 单次 imp.Event 投递。
type importEventMsg struct {
	reqID int
	ev    imp.Event
	ch    <-chan imp.Event // 同一通道继续监听下一条
}

// importClosedMsg 事件通道关闭（导入 goroutine 停止）信号。无论停在终态还是 awaiting 处，
// 通道关闭都靠它可靠告知面板可关闭，避免只认终态导致 awaiting 停机后面板卡死。
type importClosedMsg struct {
	reqID int
}

// startImport 启动一次外部小说导入：解析参数 → 创建 modal state → 监听事件流。
// width/height 用于初始化 viewport；cancel 函数挂在 state 上供 Esc 取消。
func startImport(rt *host.Host, reqID int, args []string, width, height int) (*importState, tea.Cmd, error) {
	opts, err := parseImportArgs(args)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := rt.ImportFrom(ctx, opts)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	state := newImportState(reqID, opts.SourcePath, width, height, cancel)
	return state, listenImportEvent(reqID, ch), nil
}

func listenImportEvent(reqID int, ch <-chan imp.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return importClosedMsg{reqID: reqID}
		}
		return importEventMsg{reqID: reqID, ev: ev, ch: ch}
	}
}

// parseImportArgs 解析 `/import <path> [--yes] [--story=open|closed] [--continue] [--guide=<说明>]`。
// 无参数视为“从活动工作区恢复”，源路径不是恢复必需项（RFC §18）。
// --guide 是自然语言切分指导，可含空格：从 --guide= 起其后全部内容并入指导文本，须置于最后。
func parseImportArgs(args []string) (imp.Options, error) {
	var opts imp.Options
	for i := range args {
		a := args[i]
		switch {
		case a == "--yes":
			opts.AutoConfirm = true
		case a == "--continue":
			opts.ContinueAfter = true
		case strings.HasPrefix(a, "--story="):
			v := strings.TrimPrefix(a, "--story=")
			if v != "open" && v != "closed" {
				return imp.Options{}, fmt.Errorf("--story 只能是 open 或 closed：%q", v)
			}
			opts.StoryResolution = v
		case strings.HasPrefix(a, "--guide="):
			parts := append([]string{strings.TrimPrefix(a, "--guide=")}, args[i+1:]...)
			g := strings.TrimSpace(strings.Join(parts, " "))
			if g == "" {
				return imp.Options{}, fmt.Errorf("--guide 需要自然语言切分指导，例如 --guide=幕间·X 也是独立章节")
			}
			opts.Guidance = g
			return opts, nil
		case strings.HasPrefix(a, "--"):
			return imp.Options{}, fmt.Errorf("未知选项 %q（支持：--yes / --story=open|closed / --continue / --guide=<切分指导>）", a)
		default:
			if opts.SourcePath != "" {
				return imp.Options{}, fmt.Errorf("只接受一个源文件路径：多了 %q", a)
			}
			opts.SourcePath = a
		}
	}
	return opts, nil
}
