package bridge

import (
	"strings"
	"unicode/utf8"
)

const (
	// MaxChunkSize is the maximum size of a Discord message chunk.
	// Discord limit is 2000; we leave headroom.
	MaxChunkSize = 1800
)

// ChunkText splits text into Discord-safe chunks (≤ MaxChunkSize chars).
// It tries to split on:
// 1. Blank lines (paragraph breaks)
// 2. Sentence boundaries (. ! ?)
// 3. Line breaks
// 4. Word boundaries (spaces)
// 5. Hard cut as last resort
//
// Code fences are balanced: if a chunk ends inside a code block,
// a closing ``` is appended and the next chunk reopens the block.
func ChunkText(text string) []string {
	if utf8.RuneCountInString(text) <= MaxChunkSize {
		return []string{text}
	}

	var chunks []string
	remaining := text
	inCodeBlock := false
	codeLang := ""

	for utf8.RuneCountInString(remaining) > 0 {
		if utf8.RuneCountInString(remaining) <= MaxChunkSize {
			chunks = append(chunks, remaining)
			break
		}

		// Reserve space for potential fence closing
		limit := MaxChunkSize
		if inCodeBlock {
			limit -= 5 // room for "\n```"
		}

		cutAt := findSplitPoint(remaining, limit)
		if cutAt <= 0 {
			// Absolute fallback — hard cut at byte position
			cutAt = runeToBytePos(remaining, limit)
		}

		chunk := remaining[:cutAt]
		remaining = remaining[cutAt:]

		// Track code fence state through this chunk
		chunkInCode, chunkLang := trackFences(chunk, inCodeBlock, codeLang)

		if chunkInCode {
			// We're ending inside a code block — close it
			chunk = strings.TrimRight(chunk, "\n") + "\n```"
			// Next chunk reopens
			remaining = "```" + codeLang + "\n" + strings.TrimLeft(remaining, "\n")
			inCodeBlock = true
		} else {
			inCodeBlock = false
			codeLang = chunkLang
		}

		chunks = append(chunks, strings.TrimRight(chunk, "\n"))
		if !inCodeBlock {
			remaining = strings.TrimLeft(remaining, "\n")
		}
	}

	return chunks
}

// trackFences scans a chunk and returns whether we end inside an open code block.
func trackFences(text string, startInCode bool, startLang string) (inCode bool, lang string) {
	inCode = startInCode
	lang = startLang
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inCode {
				// Opening fence
				lang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				inCode = true
			} else {
				// Closing fence
				inCode = false
			}
		}
	}
	return
}

func findSplitPoint(text string, maxRuneLen int) int {
	byteLimit := runeToBytePos(text, maxRuneLen)
	if byteLimit >= len(text) {
		return len(text)
	}
	window := text[:byteLimit]

	// Try: blank line
	if idx := strings.LastIndex(window, "\n\n"); idx > 0 {
		return idx + 1
	}

	// Try: sentence boundary
	if idx := lastSentenceBoundary(window); idx > 0 {
		return idx
	}

	// Try: line break
	if idx := strings.LastIndex(window, "\n"); idx > 0 {
		return idx + 1
	}

	// Try: word boundary
	if idx := strings.LastIndex(window, " "); idx > 0 {
		return idx + 1
	}

	// Hard cut
	return byteLimit
}

func runeToBytePos(s string, runeLimit int) int {
	n := 0
	for i := range s {
		if n >= runeLimit {
			return i
		}
		n++
	}
	return len(s)
}

func lastSentenceBoundary(text string) int {
	for i := len(text) - 1; i > 0; i-- {
		if (text[i] == '.' || text[i] == '!' || text[i] == '?') &&
			i+1 < len(text) && (text[i+1] == ' ' || text[i+1] == '\n') {
			return i + 1
		}
	}
	return -1
}
