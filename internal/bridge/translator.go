package bridge

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jelmersnoeck/forge/internal/types"
)

// DiscordAction represents an action the translator wants the bridge to perform.
type DiscordAction struct {
	Type ActionType

	// For post/edit
	ThreadID  string
	Content   string
	Embed     *discordgo.MessageEmbed
	Pin       bool

	// For reactions
	MessageID string
	Emoji     string
}

// ActionType enumerates translator output actions.
type ActionType int

const (
	ActionPost          ActionType = iota
	ActionEdit
	ActionEditEmbed
	ActionReact
	ActionRemoveReact
	ActionPin
)

// Translator accumulates Forge events and produces Discord actions.
type Translator struct {
	threadID      string
	showThinking  bool
	revealSession bool

	textBuf       strings.Builder
	lastBotMsgID  string
	starterMsgID  string
	toolMsgID     string // current tool_use embed message
	toolName      string
	thinkingShown bool
	lastPhase     string

	// Usage accumulation for the done summary
	totalInput    int
	totalOutput   int
	totalCacheIn  int
	totalCacheOut int
	models        map[string]bool
	prURL         string
	startTime     time.Time
	sessionID     string
}

// NewTranslator creates a translator for a specific thread.
func NewTranslator(threadID, starterMsgID, sessionID string, showThinking, revealSession bool) *Translator {
	return &Translator{
		threadID:      threadID,
		starterMsgID:  starterMsgID,
		sessionID:     sessionID,
		showThinking:  showThinking,
		revealSession: revealSession,
		models:        make(map[string]bool),
		startTime:     time.Now(),
	}
}

// SetLastBotMsgID sets the most recent bot message ID (for reactions).
func (tr *Translator) SetLastBotMsgID(id string) {
	tr.lastBotMsgID = id
}

// Translate converts a Forge OutboundEvent into zero or more Discord actions.
func (tr *Translator) Translate(evt types.OutboundEvent) []DiscordAction {
	switch evt.Type {
	case "text":
		return tr.onText(evt)
	case "tool_use":
		return tr.onToolUse(evt)
	case "thinking":
		return tr.onThinking(evt)
	case "intent_classified", "planning_start", "ideation_start":
		return tr.onPhaseTransition(evt)
	case "clarification_question":
		return tr.onClarification(evt)
	case "staleness_warning":
		return tr.onStalenessWarning(evt)
	case "phase_error", "error":
		return tr.onError(evt)
	case "interrupted":
		return tr.onInterrupted(evt)
	case "usage":
		return tr.onUsage(evt)
	case "pr_url":
		return tr.onPRURL(evt)
	case "done":
		return tr.onDone(evt)
	case "retry", "compact", "pr_monitor", "task_status":
		// Silent
		return nil
	default:
		// Unknown event types are silently ignored
		return nil
	}
}

func (tr *Translator) onText(evt types.OutboundEvent) []DiscordAction {
	tr.textBuf.WriteString(evt.Content)

	// If buffer is large enough, flush in chunks
	if tr.textBuf.Len() >= MaxChunkSize {
		return tr.flushTextBuffer()
	}

	// Don't flush yet — wait for done or buffer overflow
	return nil
}

func (tr *Translator) onToolUse(evt types.OutboundEvent) []DiscordAction {
	var actions []DiscordAction

	// Flush any text before the tool
	actions = append(actions, tr.flushTextBuffer()...)

	// Close previous tool embed if any
	actions = append(actions, tr.closeToolEmbed()...)

	tr.toolName = evt.ToolName
	args := evt.Content
	if len(args) > 200 {
		args = args[:200] + "…"
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🔧 %s", evt.ToolName),
		Description: args,
		Color:       0x5865F2, // Discord blurple
		Footer:      &discordgo.MessageEmbedFooter{Text: "running…"},
	}

	actions = append(actions, DiscordAction{
		Type:     ActionPost,
		ThreadID: tr.threadID,
		Embed:    embed,
	})

	return actions
}

func (tr *Translator) onThinking(_ types.OutboundEvent) []DiscordAction {
	if !tr.showThinking || tr.lastBotMsgID == "" {
		return nil
	}
	tr.thinkingShown = true
	return []DiscordAction{{
		Type:      ActionReact,
		ThreadID:  tr.threadID,
		MessageID: tr.lastBotMsgID,
		Emoji:     "💭",
	}}
}

func (tr *Translator) onPhaseTransition(evt types.OutboundEvent) []DiscordAction {
	phase := phaseLabel(evt.Type)
	if phase == tr.lastPhase {
		return nil // suppress duplicate
	}
	tr.lastPhase = phase

	var actions []DiscordAction
	actions = append(actions, tr.removeThinking()...)

	content := fmt.Sprintf("🧭 %s", phase)
	if evt.Content != "" {
		content = fmt.Sprintf("🧭 %s: %s", phase, evt.Content)
	}

	actions = append(actions, DiscordAction{
		Type:     ActionPost,
		ThreadID: tr.threadID,
		Content:  content,
	})
	return actions
}

func (tr *Translator) onClarification(evt types.OutboundEvent) []DiscordAction {
	var actions []DiscordAction
	actions = append(actions, tr.removeThinking()...)
	actions = append(actions, DiscordAction{
		Type:     ActionPost,
		ThreadID: tr.threadID,
		Content:  evt.Content,
	})
	return actions
}

func (tr *Translator) onStalenessWarning(_ types.OutboundEvent) []DiscordAction {
	if tr.lastBotMsgID == "" {
		return nil
	}
	return []DiscordAction{{
		Type:      ActionReact,
		ThreadID:  tr.threadID,
		MessageID: tr.lastBotMsgID,
		Emoji:     "⚠️",
	}}
}

func (tr *Translator) onError(evt types.OutboundEvent) []DiscordAction {
	var actions []DiscordAction
	actions = append(actions, tr.flushTextBuffer()...)
	actions = append(actions, DiscordAction{
		Type:     ActionPost,
		ThreadID: tr.threadID,
		Content:  fmt.Sprintf("❌ %s", evt.Content),
		Pin:      true,
	})
	return actions
}

func (tr *Translator) onInterrupted(_ types.OutboundEvent) []DiscordAction {
	var actions []DiscordAction
	actions = append(actions, tr.flushTextBuffer()...)
	actions = append(actions, tr.removeThinking()...)
	actions = append(actions, DiscordAction{
		Type:     ActionPost,
		ThreadID: tr.threadID,
		Content:  "⏸ Interrupted by user.",
	})
	return actions
}

func (tr *Translator) onUsage(evt types.OutboundEvent) []DiscordAction {
	if evt.Usage != nil {
		tr.totalInput += evt.Usage.InputTokens
		tr.totalOutput += evt.Usage.OutputTokens
		tr.totalCacheIn += evt.Usage.CacheCreationTokens
		tr.totalCacheOut += evt.Usage.CacheReadTokens
	}
	if evt.Model != "" {
		tr.models[evt.Model] = true
	}
	return nil
}

func (tr *Translator) onPRURL(evt types.OutboundEvent) []DiscordAction {
	tr.prURL = evt.Content
	var actions []DiscordAction
	actions = append(actions, DiscordAction{
		Type:     ActionPost,
		ThreadID: tr.threadID,
		Content:  fmt.Sprintf("🚀 PR ready: %s", evt.Content),
		Pin:      true,
	})
	// React on starter message
	if tr.starterMsgID != "" {
		actions = append(actions, DiscordAction{
			Type:      ActionReact,
			ThreadID:  tr.threadID,
			MessageID: tr.starterMsgID,
			Emoji:     "🚀",
		})
	}
	return actions
}

func (tr *Translator) onDone(_ types.OutboundEvent) []DiscordAction {
	var actions []DiscordAction

	// Flush remaining text
	actions = append(actions, tr.flushTextBuffer()...)
	// Close any open tool embed
	actions = append(actions, tr.closeToolEmbed()...)
	// Remove thinking reaction
	actions = append(actions, tr.removeThinking()...)

	// Build summary embed
	elapsed := time.Since(tr.startTime).Round(time.Second)
	modelList := make([]string, 0, len(tr.models))
	for m := range tr.models {
		modelList = append(modelList, m)
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Tokens", Value: fmt.Sprintf("↑ %d  ↓ %d", tr.totalInput, tr.totalOutput), Inline: true},
		{Name: "Duration", Value: elapsed.String(), Inline: true},
	}
	if len(modelList) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "Models", Value: strings.Join(modelList, ", "), Inline: true,
		})
	}
	if tr.prURL != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "PR", Value: tr.prURL,
		})
	}

	desc := "Session complete."
	if tr.revealSession {
		desc = fmt.Sprintf("Session `%s` complete.", tr.sessionID)
	}

	embed := &discordgo.MessageEmbed{
		Title:       "✅ Done",
		Description: desc,
		Color:       0x57F287, // green
		Fields:      fields,
	}

	actions = append(actions, DiscordAction{
		Type:     ActionPost,
		ThreadID: tr.threadID,
		Embed:    embed,
	})

	return actions
}

// flushTextBuffer posts any accumulated text, chunking if needed.
func (tr *Translator) flushTextBuffer() []DiscordAction {
	text := strings.TrimSpace(tr.textBuf.String())
	tr.textBuf.Reset()
	if text == "" {
		return nil
	}

	var actions []DiscordAction
	// Remove thinking before posting text
	actions = append(actions, tr.removeThinking()...)

	chunks := ChunkText(text)
	for _, chunk := range chunks {
		actions = append(actions, DiscordAction{
			Type:     ActionPost,
			ThreadID: tr.threadID,
			Content:  chunk,
		})
	}
	return actions
}

// closeToolEmbed edits the current tool embed to show completion.
func (tr *Translator) closeToolEmbed() []DiscordAction {
	if tr.toolMsgID == "" {
		return nil
	}

	embed := &discordgo.MessageEmbed{
		Title:  fmt.Sprintf("🔧 %s", tr.toolName),
		Color:  0x57F287,
		Footer: &discordgo.MessageEmbedFooter{Text: "✓ done"},
	}

	actions := []DiscordAction{{
		Type:      ActionEditEmbed,
		ThreadID:  tr.threadID,
		MessageID: tr.toolMsgID,
		Embed:     embed,
	}}

	tr.toolMsgID = ""
	tr.toolName = ""
	return actions
}

func (tr *Translator) removeThinking() []DiscordAction {
	if !tr.thinkingShown || tr.lastBotMsgID == "" {
		return nil
	}
	tr.thinkingShown = false
	return []DiscordAction{{
		Type:      ActionRemoveReact,
		ThreadID:  tr.threadID,
		MessageID: tr.lastBotMsgID,
		Emoji:     "💭",
	}}
}

func phaseLabel(eventType string) string {
	switch eventType {
	case "intent_classified":
		return "Intent classified"
	case "planning_start":
		return "Planning"
	case "ideation_start":
		return "Ideation"
	default:
		return eventType
	}
}
