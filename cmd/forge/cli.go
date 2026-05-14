package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/jelmersnoeck/forge/internal/runtime/cost"
	"github.com/jelmersnoeck/forge/internal/types"
)

var (
	headerStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dimStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	toolStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	errorStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	promptStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	queueStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	queueHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))
	thinkingStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Italic(true)
	userMsgStyle     = lipgloss.NewStyle().
				Background(lipgloss.Color("236")).
				Foreground(lipgloss.Color("15")).
				Padding(0, 1).
				Bold(true)
	inputBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("8")).
				Padding(0, 1)
)

// taskTracker holds live state for a background task being polled by the LLM.
// Instead of showing repeated [TaskGet] lines, the CLI renders a single
// spinner + description block with rolling output for each tracked task.
type taskTracker struct {
	taskID      string
	description string
	status      string
	outputTail  []string  // last 5 lines of output
	startTime   time.Time // for duration display on completion
	duration    string    // set when terminal
}

type model struct {
	gateway         string
	sessionID       string
	interactiveMode bool // true if talking directly to agent, false if via gateway
	textArea        textarea.Model
	queue           []string
	output          []string
	ready           bool
	quitting        bool
	exitAttempts    int    // track number of exit attempts
	working         bool   // track if agent is currently working
	thinking        bool   // track if agent is currently thinking
	toolProgress    string // current tool progress message (cleared on next event)
	spinnerFrame    int    // spinner animation frame
	width           int
	height          int
	renderer        *glamour.TermRenderer
	textBuf         string
	err             error
	scrollOffset    int  // how many lines scrolled up from bottom
	autoScroll      bool // auto-scroll to bottom on new content

	// Cost tracking
	totalUsage  types.TokenUsage // session total usage
	lastTracked types.TokenUsage // last tracked usage (for delta calculation)
	modelName   string           // model name for cost calculation
	costTracker *cost.Tracker    // persistent cost tracker

	// Worktree info
	worktreePath   string // path to worktree if created
	worktreeBranch string // branch name if worktree created
	cwd            string // working directory (worktree or original)

	// Session title (human-readable, may differ from sessionID)
	sessionTitle   string
	titleGenerated bool // whether we've already tried generating a title

	// Spec mode
	initialPrompt string // auto-sent on startup (e.g. from --spec)

	// PR tracking
	prURL string // PR URL from pr_monitor or orchestrator finalize

	// Inline task progress — keyed by task/agent ID
	taskTrackers     map[string]*taskTracker
	taskTrackerOrder []string // insertion order for stable rendering

	// cursor blink cmd captured from ta.Focus() before model creation
	cursorBlinkCmd tea.Cmd
}

type serverEvent types.OutboundEvent
type errMsg error
type tickMsg time.Time
type sessionTitleMsg string // async session title from Haiku

func runCLI(args []string) int {
	fs := flag.NewFlagSet("forge", flag.ExitOnError)
	resume := fs.String("resume", "", "session ID to resume")
	gatewayFlag := fs.String("gateway", "", "connect to remote forge gateway (e.g. http://localhost:3000)")
	serverFlag := fs.String("server", "", "deprecated: use --gateway instead")
	skipWorktree := fs.Bool("skip-worktree", false, "skip worktree creation in interactive mode")
	specPath := fs.String("spec", "", "path to a spec file to implement directly")
	branch := fs.String("branch", "", "branch to check out (reuses existing worktree if found)")
	mode := fs.String("mode", "", "agent mode: swe (default), spec, code, review")
	_ = fs.Parse(args[1:])

	if *branch != "" && *skipWorktree {
		fmt.Fprintln(os.Stderr, errorStyle.Render("cannot use --branch with --skip-worktree"))
		os.Exit(1)
	}

	// Validate --mode flag.
	switch *mode {
	case "", "swe", "spec", "code", "review":
		// valid
	default:
		fmt.Fprintln(os.Stderr, errorStyle.Render("invalid --mode: "+*mode+" (valid: swe, spec, code, review)"))
		os.Exit(1)
	}

	// Validate mode/spec conflicts.
	if *mode == "spec" && *specPath != "" {
		fmt.Fprintln(os.Stderr, errorStyle.Render("cannot use --spec with --mode spec (architect writes specs, it doesn't consume them)"))
		os.Exit(1)
	}
	if *mode == "code" && *specPath == "" {
		fmt.Fprintln(os.Stderr, errorStyle.Render("coder mode requires a spec (use --spec path/to/spec.md)"))
		os.Exit(1)
	}

	// Default mode: always swe. The orchestrator runs spec → code → review.
	effectiveMode := *mode
	if effectiveMode == "" {
		effectiveMode = "swe"
	}

	// Handle deprecated --server flag
	if *serverFlag != "" {
		fmt.Fprintln(os.Stderr, "note: --server is deprecated, use --gateway")
		if *gatewayFlag == "" {
			*gatewayFlag = *serverFlag
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, errorStyle.Render("could not determine working directory: "+err.Error()))
		os.Exit(1)
	}

	// Determine mode: interactive (default) or remote gateway
	var sessionID string
	var gatewayURL string
	var agentCleanup func()
	var worktreePath string
	var worktreeBranch string

	// Handle --spec: read spec file and prepare initial prompt.
	// Done early so the prompt is available for session naming.
	var initialPrompt string
	if *specPath != "" {
		specContent, err := os.ReadFile(*specPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, errorStyle.Render("could not read spec file: "+err.Error()))
			os.Exit(1)
		}
		initialPrompt = fmt.Sprintf(
			"Implement the following spec. The spec is the source of truth "+
				"— follow its Behavior, Constraints, and Interfaces sections precisely.\n\n"+
				"Spec file: %s\n\n%s\n\n"+
				"When done, reconcile this spec file with any corrections or "+
				"discoveries made during implementation.",
			*specPath, string(specContent),
		)
	}

	if *gatewayFlag != "" {
		// Remote gateway mode
		gatewayURL = *gatewayFlag
		if *resume != "" {
			sessionID = *resume
		} else {
			sid, err := createSession(gatewayURL, cwd)
			if err != nil {
				fmt.Fprintln(os.Stderr, errorStyle.Render("could not connect to forge gateway at "+gatewayURL))
				fmt.Fprintf(os.Stderr, "  %v\n\nhint: start the gateway with `just dev-gateway`\n", err)
				os.Exit(1)
			}
			sessionID = sid
		}
	} else {
		// Interactive mode (default) - spawn local agent
		if *resume != "" {
			fmt.Fprintln(os.Stderr, errorStyle.Render("cannot resume in interactive mode"))
			fmt.Fprintln(os.Stderr, "  hint: use --gateway to connect to a persistent gateway")
			os.Exit(1)
		}

		sid, url, wtPath, wtBranch, cleanup, err := spawnLocalAgent(cwd, *skipWorktree, *branch, initialPrompt, effectiveMode, *specPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, errorStyle.Render("failed to spawn local agent: "+err.Error()))
			os.Exit(1)
		}
		sessionID = sid
		gatewayURL = url
		worktreePath = wtPath
		worktreeBranch = wtBranch
		agentCleanup = cleanup
		defer agentCleanup()

		// Setup signal handler to ensure cleanup on interrupt
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			agentCleanup()
			os.Exit(0)
		}()
	}

	// Renderer will be initialized with proper width after first WindowSizeMsg
	renderer, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80), // initial fallback, will be updated
	)

	// Initialize cost tracker
	costTracker, err := cost.NewTracker()
	if err != nil {
		fmt.Fprintln(os.Stderr, errorStyle.Render("warning: could not initialize cost tracker: "+err.Error()))
		// Continue without cost tracking
	}

	// Initialize textarea for multiline input
	ta := textarea.New()
	ta.Placeholder = "Type your message... (Shift+Enter for newline)"
	ta.CharLimit = 0 // no limit
	ta.ShowLineNumbers = false
	ta.Prompt = "> "
	ta.SetHeight(1)
	ta.MaxHeight = 10
	ta.SetWidth(76) // will be updated on WindowSizeMsg
	ta.EndOfBufferCharacter = ' '
	// Remap: Shift+Enter inserts newline, Enter is handled by us for sending
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		key.WithHelp("shift+enter", "insert newline"),
	)
	// Disable textarea's internal viewport scrolling keys — we handle scrolling ourselves
	ta.KeyMap.LineNext = key.NewBinding(key.WithKeys("down"), key.WithHelp("down", ""))
	ta.KeyMap.LinePrevious = key.NewBinding(key.WithKeys("up"), key.WithHelp("up", ""))
	// Style: no visible border (the outer inputBorderStyle provides the border)
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = promptStyle.Inline(true)
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.Prompt = promptStyle.Inline(true)
	cursorBlinkCmd := ta.Focus()

	// Determine effective working directory
	effectiveCWD := cwd
	if worktreePath != "" {
		effectiveCWD = worktreePath
	}

	// Try to detect an existing PR for the current branch.
	prURL := detectCurrentPR(effectiveCWD)

	m := model{
		gateway:         gatewayURL,
		sessionID:       sessionID,
		interactiveMode: (*gatewayFlag == ""),
		textArea:        ta,
		output:          []string{},
		queue:           []string{},
		renderer:        renderer,
		autoScroll:      true, // start with auto-scroll enabled
		costTracker:     costTracker,
		worktreePath:    worktreePath,
		worktreeBranch:  worktreeBranch,
		cwd:             effectiveCWD,
		initialPrompt:   initialPrompt,
		working:         initialPrompt != "", // show spinner immediately for --spec
		sessionTitle:    sessionID,
		titleGenerated:  initialPrompt != "", // already named via Haiku if we had a prompt
		prURL:           prURL,
		taskTrackers:    make(map[string]*taskTracker),
		cursorBlinkCmd:  cursorBlinkCmd,
	}

	// Add welcome message
	modeDesc := "interactive"
	resumeHint := ""
	if *gatewayFlag != "" {
		modeDesc = "remote"
		resumeHint = dimStyle.Render("Press Ctrl+C to interrupt work, twice to exit. Resume: forge --gateway " + *gatewayFlag + " --resume " + sessionID)
	} else {
		if worktreeBranch != "" {
			resumeHint = dimStyle.Render("Press Ctrl+C to interrupt work, twice to exit. Resume: forge --branch " + worktreeBranch)
		} else {
			resumeHint = dimStyle.Render("Press Ctrl+C to interrupt work, twice to exit.")
		}
	}

	m.output = append(m.output,
		headerStyle.Render("forge cli")+" "+dimStyle.Render("— "+modeDesc+" — "+m.sessionTitle),
		dimStyle.Render("gateway: "+gatewayURL),
	)

	// Add worktree info if present
	if worktreePath != "" {
		m.output = append(m.output, dimStyle.Render("worktree: "+worktreePath))
		if worktreeBranch != "" {
			m.output = append(m.output, dimStyle.Render("branch: "+worktreeBranch))
		}
	}

	// Add mode info if not default
	if effectiveMode != "" {
		m.output = append(m.output, dimStyle.Render("mode: "+effectiveMode))
	}

	// Add spec info if present
	if *specPath != "" {
		m.output = append(m.output, dimStyle.Render("spec: "+*specPath))
		m.working = true // mark as working since we'll auto-send
	}

	m.output = append(m.output,
		"",
		resumeHint,
		"",
	)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	// Start SSE listener in background
	go listenEvents(p, gatewayURL, sessionID, m.interactiveMode)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	return 0
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{tick()}
	if m.cursorBlinkCmd != nil {
		cmds = append(cmds, m.cursorBlinkCmd)
	}
	if m.initialPrompt != "" {
		cmds = append(cmds, m.sendMessage(m.initialPrompt))
	}
	return tea.Batch(cmds...)
}

func tick() tea.Cmd {
	return tea.Tick(time.Millisecond*100, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.working || len(m.taskTrackers) > 0 {
			m.spinnerFrame++
		}
		return m, tick()

	case sessionTitleMsg:
		if title := string(msg); title != "" {
			m.sessionTitle = title
			// Update the header line (first line of output)
			if len(m.output) > 0 {
				modeDesc := "interactive"
				if !m.interactiveMode {
					modeDesc = "remote"
				}
				m.output[0] = headerStyle.Render("forge cli") + " " + dimStyle.Render("— "+modeDesc+" — "+m.sessionTitle)
			}
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		// Update textarea width
		m.textArea.SetWidth(msg.Width - 6) // account for border + padding
		// Reinitialize renderer with updated width for proper wrapping
		if m.width > 0 {
			m.renderer, _ = glamour.NewTermRenderer(
				glamour.WithAutoStyle(),
				glamour.WithWordWrap(m.width-4), // account for padding/margins
			)
		}
		return m, nil

	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			// Scroll up (like pressing up arrow)
			outputHeight := m.getOutputHeight()
			maxOffset := len(m.output) - outputHeight
			if maxOffset > 0 && m.scrollOffset < maxOffset {
				m.scrollOffset += 3 // scroll 3 lines at a time for smoother trackpad feel
				if m.scrollOffset > maxOffset {
					m.scrollOffset = maxOffset
				}
				m.autoScroll = false
			}
			return m, nil

		case tea.MouseButtonWheelDown:
			// Scroll down (like pressing down arrow)
			if m.scrollOffset > 0 {
				m.scrollOffset -= 3 // scroll 3 lines at a time
				if m.scrollOffset < 0 {
					m.scrollOffset = 0
				}
				if m.scrollOffset == 0 {
					m.autoScroll = true
				}
			}
			return m, nil
		}

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			// First exit attempt: send interrupt if agent is working
			if m.exitAttempts == 0 {
				m.exitAttempts++
				if m.working {
					// Send interrupt to agent
					m.output = append(m.output, "", queueStyle.Render("⚠ Interrupting agent... (press Ctrl+C again to exit)"))
					return m, m.sendInterrupt()
				} else {
					// Not working, just show the message
					m.output = append(m.output, "", dimStyle.Render("(press Ctrl+C again to exit)"))
					return m, nil
				}
			}
			// Second exit attempt: actually quit
			m.quitting = true
			return m, tea.Quit

		case tea.KeyUp:
			// If textarea has multiple lines, let it handle cursor movement
			if m.textArea.LineCount() > 1 {
				var cmd tea.Cmd
				m.textArea, cmd = m.textArea.Update(msg)
				return m, cmd
			}
			// Scroll up one line
			outputHeight := m.getOutputHeight()
			maxOffset := len(m.output) - outputHeight
			if maxOffset > 0 && m.scrollOffset < maxOffset {
				m.scrollOffset++
				m.autoScroll = false
			}
			return m, nil

		case tea.KeyDown:
			// If textarea has multiple lines, let it handle cursor movement
			if m.textArea.LineCount() > 1 {
				var cmd tea.Cmd
				m.textArea, cmd = m.textArea.Update(msg)
				return m, cmd
			}
			// Scroll down one line
			if m.scrollOffset > 0 {
				m.scrollOffset--
				if m.scrollOffset == 0 {
					m.autoScroll = true
				}
			}
			return m, nil

		case tea.KeyTab:
			// Slash command tab-completion
			if completed := m.trySlashComplete(); completed {
				return m, nil
			}
			// Otherwise let textarea handle it
			var cmd tea.Cmd
			m.textArea, cmd = m.textArea.Update(msg)
			return m, cmd

		case tea.KeyEnter:
			if m.textArea.Value() == "" {
				return m, nil
			}
			text := m.textArea.Value()
			m.textArea.Reset()
			m.textArea.SetHeight(1) // Reset height after send
			m.exitAttempts = 0      // Reset exit attempts on new message

			// If nothing in queue and not working, send immediately without queuing
			if len(m.queue) == 0 && !m.working {
				// Check for /review command
				if isReviewCommand(text) {
					baseBranch := parseReviewBase(text)
					m.output = append(m.output, "")
					m.output = append(m.output, headerStyle.Render("Starting code review...")+" "+dimStyle.Render("("+reviewProviderSummary()+")"))
					m.working = true
					return m, m.sendReview(baseBranch)
				}

				// Display the user's message in the output with wrapping
				m.output = append(m.output, "")
				maxWidth := m.width - 7 // account for "You: "
				if maxWidth < 40 {
					maxWidth = 80
				}
				wrapped := wrapText(text, maxWidth)
				for i, line := range wrapped {
					if i == 0 {
						m.output = append(m.output, userMsgStyle.Render("You: ")+line)
					} else {
						m.output = append(m.output, "     "+line)
					}
				}
				m.working = true
				cmds := []tea.Cmd{m.sendMessage(text)}
				// Generate session title from first user message
				if !m.titleGenerated {
					m.titleGenerated = true
					cmds = append(cmds, m.generateTitle(text))
				}
				return m, tea.Batch(cmds...)
			}

			// Otherwise, add to queue (will be sent when current work completes)
			m.queue = append(m.queue, text)
			return m, nil

		default:
			// Let textarea handle all other keys (including navigation)
			var cmd tea.Cmd
			m.textArea, cmd = m.textArea.Update(msg)
			m.exitAttempts = 0 // Reset on any input
			// Auto-resize textarea height based on content
			m.resizeTextArea()
			return m, cmd
		}

	case serverEvent:
		event := types.OutboundEvent(msg)
		m.handleEvent(event)

		// Track working state
		switch event.Type {
		case "thinking":
			m.thinking = true
			m.toolProgress = ""
		case "tool_progress":
			// Keep working/thinking state, just update progress
		case "text", "tool_use", "task_status", "review_start", "review_finding",
			"phase_start", "phase_handoff":
			m.thinking = false
			m.working = true
			m.toolProgress = ""
			m.exitAttempts = 0 // reset exit attempts when work starts
		case "done", "error", "interrupted":
			m.thinking = false
			m.working = false
			m.toolProgress = ""
		}

		// Auto-scroll to bottom on new content
		if m.autoScroll {
			m.scrollOffset = 0
		}

		// If done and queue has messages, send next
		if event.Type == "done" && len(m.queue) > 0 {
			text := m.queue[0]
			m.queue = m.queue[1:]
			// Display the user's message in the output with wrapping
			maxWidth := m.width - 7 // account for "You: "
			if maxWidth < 40 {
				maxWidth = 80
			}
			wrapped := wrapText(text, maxWidth)
			for i, line := range wrapped {
				if i == 0 {
					m.output = append(m.output, userMsgStyle.Render("You: ")+line)
				} else {
					m.output = append(m.output, "     "+line)
				}
			}
			m.working = true
			return m, m.sendMessage(text)
		}

		return m, nil

	case errMsg:
		m.err = msg
		return m, nil
	}

	// Forward unhandled messages to textarea (cursor blink, etc.)
	var cmd tea.Cmd
	m.textArea, cmd = m.textArea.Update(msg)
	return m, cmd
}

func (m model) getOutputHeight() int {
	queueHeight := 0
	if len(m.queue) > 0 {
		queueHeight = len(m.queue) + 2 // header + messages + separator
	}
	thinkingHeight := 0
	if m.toolProgress != "" || m.thinking || (m.working && m.textBuf == "") {
		thinkingHeight = 1 // thinking/progress/working indicator
	}
	trackerHeight := m.taskTrackerHeight()
	inputHeight := m.textArea.LineCount() + 2 // border top/bottom + content lines
	statusHeight := 1                         // cwd + cost line (always shown)
	return m.height - queueHeight - thinkingHeight - trackerHeight - inputHeight - statusHeight - 1
}

// taskTrackerHeight returns how many terminal lines the task tracker block
// will occupy (header line + up to 5 output lines per tracked task).
func (m model) taskTrackerHeight() int {
	h := 0
	for _, id := range m.taskTrackerOrder {
		if tt, ok := m.taskTrackers[id]; ok {
			h++ // header line
			h += len(tt.outputTail)
		}
	}
	return h
}

func (m model) spinner() string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return frames[m.spinnerFrame%len(frames)]
}

func (m model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	if m.quitting {
		if m.worktreeBranch != "" {
			return dimStyle.Render("Resume: forge --branch "+m.worktreeBranch) + "\n"
		}
		return "\n"
	}

	// Calculate available space
	outputHeight := m.getOutputHeight()

	// Build output area (scrollable)
	var outputArea string
	if len(m.output) > outputHeight {
		// Calculate which slice of output to show based on scroll offset
		endIdx := len(m.output) - m.scrollOffset
		startIdx := endIdx - outputHeight
		if startIdx < 0 {
			startIdx = 0
		}
		outputArea = strings.Join(m.output[startIdx:endIdx], "\n")
	} else {
		outputArea = strings.Join(m.output, "\n")
	}

	// Build thinking/progress indicator
	// Priority: toolProgress > thinking > working (when no text streaming)
	var thinkingIndicator string
	switch {
	case m.toolProgress != "":
		thinkingIndicator = thinkingStyle.Render(m.spinner() + " " + m.toolProgress)
	case m.thinking:
		thinkingIndicator = thinkingStyle.Render(m.spinner() + " thinking...")
	case m.working && m.textBuf == "":
		thinkingIndicator = thinkingStyle.Render(m.spinner() + " working...")
	}

	// Build queue display
	var queueArea string
	if len(m.queue) > 0 {
		queueLines := []string{queueHeaderStyle.Render("Queued messages:")}
		for i, msg := range m.queue {
			prefix := "  "
			if i == 0 {
				prefix = "→ "
			}
			// Wrap queue messages instead of truncating
			maxWidth := m.width - 4 // account for prefix
			if maxWidth < 40 {
				maxWidth = 80
			}
			wrapped := wrapText(msg, maxWidth)
			for j, line := range wrapped {
				if j == 0 {
					queueLines = append(queueLines, prefix+queueStyle.Render(line))
				} else {
					// Indent continuation lines
					queueLines = append(queueLines, "  "+queueStyle.Render(line))
				}
			}
		}
		queueLines = append(queueLines, "")
		queueArea = strings.Join(queueLines, "\n")
	}

	// Build input area with textarea component
	inputArea := inputBorderStyle.Width(m.width - 4).Render(
		m.textArea.View(),
	)

	// Build status line below input: cwd (left) | PR (center) | cost (right)
	statusStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("8")).
		Faint(true)
	prStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("4")).
		Faint(true)

	cwdDisplay := m.cwd
	// Shorten home directory prefix
	if home, err := os.UserHomeDir(); err == nil {
		cwdDisplay = strings.Replace(cwdDisplay, home, "~", 1)
	}

	var costPart string
	if m.totalUsage.InputTokens > 0 || m.totalUsage.OutputTokens > 0 {
		tokens := fmt.Sprintf("in: %d | out: %d", m.totalUsage.InputTokens, m.totalUsage.OutputTokens)
		totalCost := cost.Calculate(m.modelName, m.totalUsage)
		costStr := cost.FormatCost(totalCost)
		costPart = fmt.Sprintf("%s | %s", tokens, costStr)
	}

	// Build left side: cwd + optional PR link
	leftParts := []string{"  " + cwdDisplay}
	if m.prURL != "" {
		leftParts = append(leftParts, prStyle.Render(m.prURL))
	}
	left := statusStyle.Render(strings.Join(leftParts, "  "))

	var statusLine string
	switch {
	case costPart != "":
		right := statusStyle.Render(costPart + "  ")
		gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			gap = 1
		}
		statusLine = left + strings.Repeat(" ", gap) + right
	default:
		statusLine = left
	}

	// Combine all areas
	var parts []string
	if outputArea != "" {
		parts = append(parts, outputArea)
	}
	// Task progress trackers sit between output and thinking indicator
	if trackerArea := m.renderTaskTrackers(); trackerArea != "" {
		parts = append(parts, trackerArea)
	}
	if thinkingIndicator != "" {
		parts = append(parts, thinkingIndicator)
	}
	if queueArea != "" {
		parts = append(parts, queueArea)
	}
	parts = append(parts, inputArea)
	parts = append(parts, statusLine)

	return strings.Join(parts, "\n")
}

// wrapText wraps text to fit within maxWidth, breaking on word boundaries when possible
func wrapText(text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{text}
	}

	// Handle empty or short text
	if len(text) <= maxWidth {
		return []string{text}
	}

	var lines []string
	remaining := text

	for len(remaining) > 0 {
		switch {
		case len(remaining) <= maxWidth:
			// Remaining text fits on one line
			lines = append(lines, remaining)
			remaining = ""

		default:
			// Need to break the line
			breakAt := maxWidth

			// Look for last space before maxWidth
			lastSpace := strings.LastIndex(remaining[:maxWidth], " ")
			if lastSpace > maxWidth/2 {
				// Found a reasonable break point
				breakAt = lastSpace
			}

			// Extract line and update remaining
			lines = append(lines, remaining[:breakAt])
			remaining = strings.TrimLeft(remaining[breakAt:], " ")
		}
	}

	return lines
}

func (m *model) handleEvent(event types.OutboundEvent) {
	switch event.Type {
	case "model":
		m.modelName = event.Content

	case "text":
		m.textBuf += event.Content

	case "tool_use":
		m.flushText()
		// Suppress repeated TaskGet/AgentGet lines — the inline task
		// progress display handles these via task_status events.
		switch event.ToolName {
		case "TaskGet", "AgentGet", "TaskOutput":
			// Don't render a line; the task_status event updates the tracker.
		default:
			if event.Content != "" {
				// Wrap long tool content to terminal width
				maxWidth := m.width - 10 // account for prefix and margins
				if maxWidth < 40 {
					maxWidth = 40
				}
				wrapped := wrapText(event.Content, maxWidth)
				prefix := toolStyle.Render("  ["+event.ToolName+"]") + " "
				for i, line := range wrapped {
					if i == 0 {
						m.output = append(m.output, prefix+dimStyle.Render(line))
					} else {
						// Indent continuation lines
						m.output = append(m.output, "    "+dimStyle.Render(line))
					}
				}
			} else {
				m.output = append(m.output, toolStyle.Render("  ["+event.ToolName+"]"))
			}
		}

	case "tool_progress":
		m.toolProgress = event.Content

	case "queued_task_result":
		m.flushText()
		maxWidth := m.width - 14 // account for prefix
		if maxWidth < 40 {
			maxWidth = 40
		}
		wrapped := wrapText(event.Content, maxWidth)
		for i, line := range wrapped {
			if i == 0 {
				m.output = append(m.output, queueStyle.Render("  [queued] ")+dimStyle.Render(line))
			} else {
				m.output = append(m.output, "            "+dimStyle.Render(line))
			}
		}

	case "queued_task_error":
		m.flushText()
		maxWidth := m.width - 20 // account for prefix
		if maxWidth < 40 {
			maxWidth = 40
		}
		wrapped := wrapText(event.Content, maxWidth)
		for i, line := range wrapped {
			if i == 0 {
				m.output = append(m.output, errorStyle.Render("  [queued error] ")+line)
			} else {
				m.output = append(m.output, "                  "+line)
			}
		}

	case "queue_immediate":
		m.flushText()
		maxWidth := m.width - 24 // account for prefix
		if maxWidth < 40 {
			maxWidth = 40
		}
		wrapped := wrapText(event.Content, maxWidth)
		for i, line := range wrapped {
			if i == 0 {
				m.output = append(m.output, queueStyle.Render("  ⏱  Queued immediate: ")+dimStyle.Render(line))
			} else {
				m.output = append(m.output, "                        "+dimStyle.Render(line))
			}
		}

	case "queue_on_complete":
		m.flushText()
		maxWidth := m.width - 27 // account for prefix
		if maxWidth < 40 {
			maxWidth = 40
		}
		wrapped := wrapText(event.Content, maxWidth)
		for i, line := range wrapped {
			if i == 0 {
				m.output = append(m.output, queueStyle.Render("  ⏱  Queued on complete: ")+dimStyle.Render(line))
			} else {
				m.output = append(m.output, "                           "+dimStyle.Render(line))
			}
		}

	case "usage":
		// Loop sends cumulative totalUsage
		if event.Usage != nil {
			m.totalUsage = *event.Usage
		}
		// Track model name for cost calculation
		if event.Model != "" {
			m.modelName = event.Model
		}

		// Track cost persistently (only the delta since last track)
		if m.costTracker != nil && event.Usage != nil && event.Model != "" {
			// Calculate delta from last tracked usage
			deltaUsage := types.TokenUsage{
				InputTokens:         event.Usage.InputTokens - m.lastTracked.InputTokens,
				OutputTokens:        event.Usage.OutputTokens - m.lastTracked.OutputTokens,
				CacheCreationTokens: event.Usage.CacheCreationTokens - m.lastTracked.CacheCreationTokens,
				CacheReadTokens:     event.Usage.CacheReadTokens - m.lastTracked.CacheReadTokens,
			}

			// Only track if there's a non-zero delta
			if deltaUsage.InputTokens > 0 || deltaUsage.OutputTokens > 0 ||
				deltaUsage.CacheCreationTokens > 0 || deltaUsage.CacheReadTokens > 0 {

				callCost := cost.Calculate(event.Model, deltaUsage)
				if err := m.costTracker.Track(
					m.sessionID,
					event.Model,
					deltaUsage.InputTokens,
					deltaUsage.OutputTokens,
					deltaUsage.CacheCreationTokens,
					deltaUsage.CacheReadTokens,
					callCost,
				); err != nil {
					// Don't fail the session, just log
					m.output = append(m.output, dimStyle.Render("  ⚠  cost tracking error: "+err.Error()))
				}

				// Update lastTracked to current usage
				m.lastTracked = *event.Usage
			}
		}

	case "error":
		m.flushText()
		maxWidth := m.width - 8 // account for "error: "
		if maxWidth < 40 {
			maxWidth = 40
		}
		wrapped := wrapText(event.Content, maxWidth)
		for i, line := range wrapped {
			if i == 0 {
				m.output = append(m.output, errorStyle.Render("error: ")+line)
			} else {
				m.output = append(m.output, "       "+line)
			}
		}

	case "interrupted":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("interrupted by user"))

	case "done":
		m.flushText()
		// Finalize any remaining task trackers (agent done, no more polling).
		for _, id := range append([]string{}, m.taskTrackerOrder...) {
			if tt, ok := m.taskTrackers[id]; ok {
				m.finalizeTaskTracker(tt)
			}
		}
		m.output = append(m.output, "")

	case "phase_start":
		m.flushText()
		m.output = append(m.output, "")
		m.output = append(m.output, headerStyle.Render("  [phase] ")+event.Content+" starting")

	case "phase_complete":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  [phase] ")+event.Content)
		m.output = append(m.output, "")

	case "phase_handoff":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  [phase] ")+event.Content)

	case "intent_classified":
		m.flushText()
		switch event.Content {
		case "question":
			m.output = append(m.output, dimStyle.Render("  answering question..."))
		case "investigate":
			m.output = append(m.output, dimStyle.Render("  investigating..."))
		}
		// "task" is silent — the phase_start events provide the display.

	case "classification_error":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  "+event.Content))

	case "ideation_start":
		m.flushText()
		m.output = append(m.output, headerStyle.Render("  [ideation] ")+event.Content)

	case "ideation_candidate":
		// Quiet — candidates are internal to the pipeline

	case "clarification_start":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  [clarify] ")+event.Content)

	case "clarification_question":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  [question] ")+event.Content)

	case "planning_start":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  [planning] ")+event.Content)

	case "planning_selection":
		m.flushText()
		m.output = append(m.output, headerStyle.Render("  [plan] ")+event.Content)

	case "staleness_warning":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  [staleness] ")+event.Content)

	case "staleness_error":
		m.flushText()
		m.output = append(m.output, errorStyle.Render("  [staleness] ")+event.Content)

	case "review_start":
		m.flushText()
		m.output = append(m.output, headerStyle.Render("  [review] ")+event.Content)

	case "review_finding":
		m.flushText()
		m.output = append(m.output, formatReviewFinding(event.Content, m.width))

	case "review_agent_done":
		// Quiet — individual agent completions don't need display

	case "review_provider_summary":
		m.flushText()
		m.output = append(m.output, "")
		for _, line := range strings.Split(event.Content, "\n") {
			m.output = append(m.output, "  "+dimStyle.Render(line))
		}

	case "review_summary":
		m.flushText()
		m.output = append(m.output, "")
		m.output = append(m.output, headerStyle.Render("  Review Summary"))
		for _, line := range strings.Split(event.Content, "\n") {
			m.output = append(m.output, "  "+line)
		}
		m.output = append(m.output, "")

	case "review_error":
		m.flushText()
		m.output = append(m.output, errorStyle.Render("  [review error] ")+event.Content)

	case "pr_url":
		m.prURL = event.Content

	case "pr_monitor":
		m.flushText()
		m.output = append(m.output, dimStyle.Render("  [pr] ")+event.Content)

	case "task_status":
		m.handleTaskStatus(event.Content)
	}
}

// handleTaskStatus parses a task_status event and updates (or creates) the
// inline tracker for that task. When the task reaches a terminal state the
// tracker is finalized: a one-line summary is appended to scrollback output
// and the tracker is removed so it no longer takes up screen space.
func (m *model) handleTaskStatus(content string) {
	var payload struct {
		ID          string   `json:"id"`
		Description string   `json:"description"`
		Status      string   `json:"status"`
		OutputTail  []string `json:"outputTail"`
		Duration    string   `json:"duration"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return
	}

	tt, exists := m.taskTrackers[payload.ID]
	if !exists {
		tt = &taskTracker{
			taskID:    payload.ID,
			startTime: time.Now(),
		}
		m.taskTrackers[payload.ID] = tt
		m.taskTrackerOrder = append(m.taskTrackerOrder, payload.ID)
	}

	tt.description = payload.Description
	tt.status = payload.Status
	tt.outputTail = payload.OutputTail
	tt.duration = payload.Duration

	// Terminal? Flush a final summary into scrollback and remove the tracker.
	switch tt.status {
	case "completed", "failed", "killed":
		m.finalizeTaskTracker(tt)
	}
}

// finalizeTaskTracker moves a finished task from the live tracker area into
// the scrollback output as a single summary line.
func (m *model) finalizeTaskTracker(tt *taskTracker) {
	icon := dimStyle.Render("?")
	switch tt.status {
	case "completed":
		icon = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("✓")
	case "failed":
		icon = errorStyle.Render("✗")
	case "killed":
		icon = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("⊘")
	}

	dur := tt.duration
	if dur == "" {
		dur = time.Since(tt.startTime).Round(time.Second).String()
	}

	m.output = append(m.output,
		fmt.Sprintf("  %s %s (%s) %s",
			icon,
			tt.description,
			tt.taskID,
			dimStyle.Render(dur),
		),
	)

	delete(m.taskTrackers, tt.taskID)
	// Remove from order slice.
	for i, id := range m.taskTrackerOrder {
		if id == tt.taskID {
			m.taskTrackerOrder = append(m.taskTrackerOrder[:i], m.taskTrackerOrder[i+1:]...)
			break
		}
	}
}

// renderTaskTrackers produces the live task progress block shown between
// the main output area and the thinking indicator / input box.
//
//	⠹ Running tests (b3)                 running
//	    PASS TestFoo
//	    PASS TestBar
func (m model) renderTaskTrackers() string {
	if len(m.taskTrackers) == 0 {
		return ""
	}

	var lines []string
	for _, id := range m.taskTrackerOrder {
		tt, ok := m.taskTrackers[id]
		if !ok {
			continue
		}

		// Header: spinner + description (task_id) + status
		statusColor := lipgloss.Color("6") // cyan = running
		switch tt.status {
		case "pending":
			statusColor = lipgloss.Color("8")
		}
		statusStr := lipgloss.NewStyle().Foreground(statusColor).Render(tt.status)

		header := fmt.Sprintf("  %s %s %s %s",
			thinkingStyle.Render(m.spinner()),
			tt.description,
			dimStyle.Render("("+tt.taskID+")"),
			statusStr,
		)
		lines = append(lines, header)

		// Output tail — indented
		maxWidth := m.width - 8
		if maxWidth < 40 {
			maxWidth = 40
		}
		for _, ol := range tt.outputTail {
			if len(ol) > maxWidth {
				ol = ol[:maxWidth]
			}
			lines = append(lines, "      "+dimStyle.Render(ol))
		}
	}

	return strings.Join(lines, "\n")
}
func (m *model) flushText() {
	text := m.textBuf
	m.textBuf = ""
	if text == "" {
		return
	}

	// Sniff for GitHub PR URLs if we don't already have one.
	if m.prURL == "" {
		m.prURL = extractPRURL(text)
	}

	rendered, err := m.renderer.Render(text)
	if err != nil {
		m.output = append(m.output, text)
		return
	}

	// Split into lines and add to output
	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	m.output = append(m.output, lines...)
}

func (m model) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"text": text})

		var url string
		if m.interactiveMode {
			url = fmt.Sprintf("%s/messages", m.gateway)
		} else {
			url = fmt.Sprintf("%s/sessions/%s/messages", m.gateway, m.sessionID)
		}

		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			return errMsg(err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			return errMsg(fmt.Errorf("%d %s", resp.StatusCode, b))
		}
		return nil
	}
}

// generateTitle fires an async Haiku call to produce a session title from the
// user's first message. The result updates the CLI header display only — the
// branch and worktree names are not changed after creation.
func (m model) generateTitle(prompt string) tea.Cmd {
	return func() tea.Msg {
		title := generateSessionName(newLightweightProvider(), prompt)
		return sessionTitleMsg(title)
	}
}

func (m model) sendInterrupt() tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"interrupt": "true"})

		var url string
		if m.interactiveMode {
			url = fmt.Sprintf("%s/interrupt", m.gateway)
		} else {
			url = fmt.Sprintf("%s/sessions/%s/interrupt", m.gateway, m.sessionID)
		}

		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			return errMsg(err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return errMsg(fmt.Errorf("interrupt failed: %d %s", resp.StatusCode, b))
		}
		return nil
	}
}

func createSession(gatewayURL, cwd string) (string, error) {
	body, _ := json.Marshal(map[string]string{"cwd": cwd})
	resp, err := http.Post(gatewayURL+"/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.SessionID, nil
}

func listenEvents(p *tea.Program, gatewayURL, sessionID string, interactiveMode bool) {
	var url string
	if interactiveMode {
		url = fmt.Sprintf("%s/events", gatewayURL)
	} else {
		url = fmt.Sprintf("%s/sessions/%s/events", gatewayURL, sessionID)
	}

	resp, err := http.Get(url)
	if err != nil {
		p.Send(errMsg(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var event types.OutboundEvent
		if err := json.Unmarshal([]byte(line[6:]), &event); err != nil {
			continue
		}

		p.Send(serverEvent(event))
	}

	if err := scanner.Err(); err != nil {
		p.Send(errMsg(err))
	}
}

// isDefaultBranch returns true for branches that should trigger ephemeral
// worktree mode rather than branch-reuse mode (main, master, HEAD/detached).
func isDefaultBranch(branch string) bool {
	switch branch {
	case "main", "master", "HEAD":
		return true
	}
	return false
}

// isInWorktree checks if the current directory is inside a git worktree.
// Returns false if not in a git repo or if in the main repo.
func isInWorktree(dir string) bool {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return false
	}
	// In a worktree, .git is a file (pointing to the real git dir)
	// In the main repo, .git is a directory
	return !info.IsDir()
}

// spawnLocalAgent starts a forge agent subprocess and returns (sessionID, serverURL, worktreePath, worktreeBranch, cleanup, error).
// The agent runs on a random port and auto-terminates when cleanup is called.
// If skipWorktree is false and in a git repo (and not already in a worktree), creates a temporary worktree for the session.
// If branchName is set, reuses an existing worktree for that branch or creates one.
// initialPrompt, when non-empty, is used to generate a human-readable session name via Haiku.
func spawnLocalAgent(cwd string, skipWorktree bool, branchName string, initialPrompt string, mode string, specPath string) (string, string, string, string, func(), error) {
	// Find forge binary (prefer same dir as CLI, fallback to PATH)
	forgeBin := "forge"
	if exe, err := os.Executable(); err == nil {
		// If we're already the forge binary, use ourselves
		if filepath.Base(exe) == "forge" || strings.HasPrefix(filepath.Base(exe), "forge.") {
			forgeBin = exe
		} else {
			// Look for forge in same directory
			candidate := filepath.Join(filepath.Dir(exe), "forge")
			if _, err := os.Stat(candidate); err == nil {
				forgeBin = candidate
			}
		}
	}

	// Generate session ID with a readable name.
	// If we have a prompt, ask Haiku for a slug (up to 3s).
	// Otherwise, pick a random adjective-noun pair.
	slug := generateSessionName(newLightweightProvider(), initialPrompt)
	sessionID := time.Now().Format("20060102") + "-" + slug

	// Check if we're in a git repo and should create a worktree
	var worktreePath string
	var worktreeBranch string
	var repoRoot string
	worktreeBase := filepath.Join(os.TempDir(), "forge", "worktrees")

	// explicitBranch tracks whether --branch was passed by the user (reuse ok)
	// vs auto-detected from the current checkout (always fresh worktree).
	explicitBranch := branchName != ""

	// Auto-detect branch: if no --branch flag, not skipping worktrees, and not
	// already in a worktree, check the current branch. If it's a feature branch
	// (not main/master/HEAD), create a fresh worktree branched off it instead
	// of an ephemeral one from HEAD.
	var detectedBranch string
	if branchName == "" && !skipWorktree && !isInWorktree(cwd) {
		if root := findRepoRoot(cwd); root != "" {
			cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
			cmd.Dir = root
			if out, err := cmd.Output(); err == nil {
				detected := strings.TrimSpace(string(out))
				if !isDefaultBranch(detected) {
					detectedBranch = detected
					fmt.Fprintln(os.Stderr, dimStyle.Render("  detected branch: "+detectedBranch))
				}
			}
		}
	}

	if explicitBranch {
		// Explicit --branch: find or create worktree for the named branch.
		// Reuses existing worktrees (intentional resume).
		repoRoot = findRepoRoot(cwd)
		if repoRoot == "" {
			return "", "", "", "", nil, fmt.Errorf("not in a git repo")
		}

		wtPath, err := findWorktreeForBranch(repoRoot, branchName)
		if err != nil {
			return "", "", "", "", nil, fmt.Errorf("listing worktrees: %w", err)
		}

		if wtPath != "" {
			worktreePath = wtPath
			worktreeBranch = branchName
			cwd = worktreePath
			if info, err := readSessionFile(wtPath); err == nil {
				sessionID = info.SessionID
				fmt.Fprintln(os.Stderr, dimStyle.Render("  resuming session: "+sessionID))

				// Warn if session JSONL is missing (conversation history lost)
				if jsonlPath, err := sessionFilePath(sessionID); err == nil {
					switch _, err := os.Stat(jsonlPath); {
					case os.IsNotExist(err):
						fmt.Fprintln(os.Stderr, errorStyle.Render("  warning: session history unavailable, conversation will start fresh"))
					case err != nil:
						fmt.Fprintf(os.Stderr, "  warning: could not check session history: %v\n", err)
					}
				}
			}
			fmt.Fprintln(os.Stderr, dimStyle.Render("  reusing worktree: "+worktreePath))
			fmt.Fprintln(os.Stderr, dimStyle.Render("  branch: "+branchName))
		} else {
			// No existing worktree — create one
			worktreePath = filepath.Join(worktreeBase, sessionID)
			if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
				return "", "", "", "", nil, fmt.Errorf("create worktree dir: %w", err)
			}

			// Try checking out existing branch first; if that fails, create it
			cmd := exec.Command("git", "worktree", "add", worktreePath, branchName)
			cmd.Dir = repoRoot
			if out, err := cmd.CombinedOutput(); err != nil {
				cmd = exec.Command("git", "worktree", "add", "-b", branchName, worktreePath, "HEAD")
				cmd.Dir = repoRoot
				if out2, err2 := cmd.CombinedOutput(); err2 != nil {
					return "", "", "", "", nil, fmt.Errorf("git worktree add: %s\n%s", err, string(append(out, out2...)))
				}
			}

			worktreeBranch = branchName
			cwd = worktreePath
			fmt.Fprintln(os.Stderr, dimStyle.Render("  created worktree: "+worktreePath))
			fmt.Fprintln(os.Stderr, dimStyle.Render("  branch: "+branchName))
		}
	} else if !skipWorktree && !isInWorktree(cwd) {
		// Fresh worktree mode: create a new branch from either the detected
		// feature branch or the current branch.
		repoRoot = findRepoRoot(cwd)
		if repoRoot != "" {
			baseBranch := detectedBranch
			if baseBranch == "" {
				cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
				cmd.Dir = repoRoot
				if branchOut, err := cmd.Output(); err == nil {
					baseBranch = strings.TrimSpace(string(branchOut))
				}
			}

			if baseBranch != "" {
				worktreePath = filepath.Join(worktreeBase, sessionID)
				if err := os.MkdirAll(worktreeBase, 0o755); err == nil {
					newBranch := fmt.Sprintf("jelmer/%s", sessionID)
					cmd := exec.Command("git", "worktree", "add", "-b", newBranch, worktreePath, baseBranch)
					cmd.Dir = repoRoot
					if err := cmd.Run(); err == nil {
						worktreeBranch = newBranch
						fmt.Fprintln(os.Stderr, dimStyle.Render("  created worktree: "+worktreePath))
						fmt.Fprintln(os.Stderr, dimStyle.Render("  branch: "+newBranch+" (from "+baseBranch+")"))
						cwd = worktreePath
					}
				}
			}
		}
	}

	// Write .forge-session metadata for resume
	if worktreePath != "" && repoRoot != "" {
		_ = writeSessionFile(worktreePath, SessionInfo{
			SessionID: sessionID,
			Branch:    worktreeBranch,
			RepoRoot:  repoRoot,
			CreatedAt: time.Now(),
		})
	}

	// Spawn agent subcommand on random port (0 = OS picks)
	agentArgs := []string{"agent",
		"--port", "0",
		"--cwd", cwd,
		"--session-id", sessionID,
	}
	if mode != "" {
		agentArgs = append(agentArgs, "--mode", mode)
	}
	if specPath != "" {
		agentArgs = append(agentArgs, "--spec", specPath)
	}
	cmd := exec.Command(forgeBin, agentArgs...)

	// Capture stdout to read the port
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", "", "", nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	// Send stderr to /dev/null (agent logs are noise in interactive mode)
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return "", "", "", "", nil, fmt.Errorf("start agent: %w", err)
	}

	// Read port from first line of stdout (JSON: {"port": 12345})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		_ = cmd.Process.Kill()
		return "", "", "", "", nil, fmt.Errorf("agent did not emit port")
	}

	var portMsg struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &portMsg); err != nil {
		_ = cmd.Process.Kill()
		return "", "", "", "", nil, fmt.Errorf("parse agent port: %w", err)
	}

	serverURL := fmt.Sprintf("http://localhost:%d", portMsg.Port)

	// Wait for agent to be ready (health check with retries)
	ready := false
	for i := 0; i < 10; i++ {
		resp, err := http.Get(serverURL + "/health")
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			ready = true
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !ready {
		_ = cmd.Process.Kill()
		return "", "", "", "", nil, fmt.Errorf("agent did not become healthy")
	}

	// Track whether cleanup has been called to avoid double-cleanup
	var cleanupCalled bool
	var cleanupMutex sync.Mutex

	cleanup := func() {
		cleanupMutex.Lock()
		defer cleanupMutex.Unlock()

		if cleanupCalled {
			return
		}
		cleanupCalled = true

		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}

		// Print resume hint if worktree is preserved
		if worktreePath != "" && worktreeBranch != "" {
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, dimStyle.Render("  worktree preserved: "+worktreePath))
			fmt.Fprintln(os.Stderr, dimStyle.Render("  resume: forge --branch "+worktreeBranch))
		}
	}

	return sessionID, serverURL, worktreePath, worktreeBranch, cleanup, nil
}

// findRepoRoot returns the git repository root for the given directory, or ""
// if the directory is not inside a git repo.
func findRepoRoot(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findWorktreeForBranch parses `git worktree list --porcelain` and returns the
// worktree path whose checked-out branch matches the given name, or "" if none.
//
//	worktree /tmp/forge/worktrees/cli-20260406-183659
//	HEAD abc123
//	branch refs/heads/jelmer/cli-20260406-183659
//	<blank line>
func findWorktreeForBranch(repoRoot, branch string) (string, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	target := "refs/heads/" + branch
	var currentPath string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			currentPath = strings.TrimPrefix(line, "worktree ")
		case strings.TrimSpace(line) == "":
			currentPath = ""
		case line == "branch "+target:
			return currentPath, nil
		}
	}
	return "", nil
}

// trySlashComplete attempts tab-completion of slash commands.
// Returns true if a completion was applied.
func (m *model) trySlashComplete() bool {
	val := m.textArea.Value()
	if !strings.HasPrefix(val, "/") {
		return false
	}
	// Find matching commands
	var matches []string
	for _, name := range slashCommandNames() {
		if strings.HasPrefix(name, val) {
			matches = append(matches, name)
		}
	}
	if len(matches) == 1 {
		m.textArea.SetValue(matches[0])
		return true
	}
	return false
}

// resizeTextArea adjusts the textarea height to fit content, clamped to [1, 10].
func (m *model) resizeTextArea() {
	lines := m.textArea.LineCount()
	h := lines
	if h < 1 {
		h = 1
	}
	if h > 10 {
		h = 10
	}
	m.textArea.SetHeight(h)
}

// prURLRe matches GitHub pull request URLs in text.
var prURLRe = regexp.MustCompile(`https://github\.com/[^\s/]+/[^\s/]+/pull/\d+`)

// extractPRURL finds the first GitHub PR URL in text, or returns "".
func extractPRURL(text string) string {
	return prURLRe.FindString(text)
}

// detectCurrentPR checks if the current branch has an open PR on GitHub.
// Returns the PR URL or "" if none found (no gh, no repo, no PR — all silent).
func detectCurrentPR(cwd string) string {
	cmd := exec.Command("gh", "pr", "view", "--json", "url", "--jq", ".url")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isReviewCommand checks if the input is a /review command.
func isReviewCommand(text string) bool {
	trimmed := strings.TrimSpace(text)
	return trimmed == "/review" || strings.HasPrefix(trimmed, "/review ")
}

// parseReviewBase extracts the --base flag from a /review command.
// "/review --base main" → "main", "/review" → ""
func parseReviewBase(text string) string {
	parts := strings.Fields(text)
	for i, part := range parts {
		if part == "--base" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// reviewProviderSummary returns a human-readable summary of available review providers.
func reviewProviderSummary() string {
	var providers []string
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		providers = append(providers, "Anthropic")
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		providers = append(providers, "OpenAI")
	}
	if len(providers) == 0 {
		return "no providers configured"
	}
	return strings.Join(providers, " + ")
}

// sendReview sends a review request to the agent/gateway.
func (m model) sendReview(baseBranch string) tea.Cmd {
	return func() tea.Msg {
		body, _ := json.Marshal(map[string]string{"base": baseBranch})

		var url string
		switch m.interactiveMode {
		case true:
			url = fmt.Sprintf("%s/review", m.gateway)
		default:
			url = fmt.Sprintf("%s/sessions/%s/review", m.gateway, m.sessionID)
		}

		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			return errMsg(err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			return errMsg(fmt.Errorf("review request failed: %d %s", resp.StatusCode, b))
		}
		return nil
	}
}

// formatReviewFinding formats a review finding for display.
func formatReviewFinding(content string, width int) string {
	// Content is JSON of review.Finding — parse it for nice display.
	var finding struct {
		Reviewer    string `json:"reviewer"`
		Provider    string `json:"provider"`
		Severity    string `json:"severity"`
		File        string `json:"file"`
		StartLine   int    `json:"startLine"`
		EndLine     int    `json:"endLine"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(content), &finding); err != nil {
		return dimStyle.Render("  [finding] ") + content
	}

	severityStyle := dimStyle
	severityIcon := " "
	switch finding.Severity {
	case "critical":
		severityStyle = errorStyle
		severityIcon = "!!"
	case "warning":
		severityStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
		severityIcon = " !"
	case "suggestion":
		severityStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
		severityIcon = " ~"
	}

	header := severityStyle.Render(severityIcon+" ["+finding.Severity+"]") + " " +
		dimStyle.Render(finding.Reviewer+" ("+finding.Provider+")")

	location := ""
	if finding.File != "" {
		location = dimStyle.Render(" " + finding.File)
		if finding.StartLine > 0 {
			location += dimStyle.Render(fmt.Sprintf(":%d", finding.StartLine))
			if finding.EndLine > 0 && finding.EndLine != finding.StartLine {
				location += dimStyle.Render(fmt.Sprintf("-%d", finding.EndLine))
			}
		}
	}

	maxDescWidth := width - 6
	if maxDescWidth < 40 {
		maxDescWidth = 40
	}
	descLines := wrapText(finding.Description, maxDescWidth)

	var result string
	result = header + location
	for _, line := range descLines {
		result += "\n    " + line
	}
	return result
}
