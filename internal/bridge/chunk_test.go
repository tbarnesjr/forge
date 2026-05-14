package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestChunkText_ShortText(t *testing.T) {
	text := "Hello, this is a short message."
	chunks := ChunkText(text)
	require.Len(t, chunks, 1)
	require.Equal(t, text, chunks[0])
}

func TestChunkText_ExactLimit(t *testing.T) {
	text := strings.Repeat("a", MaxChunkSize)
	chunks := ChunkText(text)
	require.Len(t, chunks, 1)
}

func TestChunkText_LongText_SplitsOnSentence(t *testing.T) {
	// Build text that exceeds MaxChunkSize
	sentence := "This is a sentence. "
	var b strings.Builder
	for b.Len() < MaxChunkSize+500 {
		b.WriteString(sentence)
	}
	text := b.String()

	chunks := ChunkText(text)
	require.Greater(t, len(chunks), 1)
	for _, chunk := range chunks {
		require.LessOrEqual(t, utf8.RuneCountInString(chunk), MaxChunkSize+10, // +10 for fence closing
			"chunk exceeds limit: len=%d", utf8.RuneCountInString(chunk))
	}
	// Reassembled text should contain all content
	joined := strings.Join(chunks, "\n")
	require.Contains(t, joined, "This is a sentence")
}

func TestChunkText_CodeBlock_BalancedFences(t *testing.T) {
	// A code block that spans the chunk boundary
	var b strings.Builder
	b.WriteString("Before code:\n")
	b.WriteString("```go\n")
	for i := 0; i < 100; i++ {
		b.WriteString("func foo() { return 42 }\n")
	}
	b.WriteString("```\n")
	b.WriteString("After code.\n")

	text := b.String()
	require.Greater(t, utf8.RuneCountInString(text), MaxChunkSize)

	chunks := ChunkText(text)
	require.Greater(t, len(chunks), 1)

	// Every chunk that opens a code fence should close it
	for i, chunk := range chunks {
		opens := strings.Count(chunk, "```")
		require.Equal(t, 0, opens%2,
			"chunk %d has unbalanced fences (%d total)", i, opens)
	}
}

func TestChunkText_MixedContent(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Header\n\n")
	b.WriteString("Some paragraph text that explains things.\n\n")
	b.WriteString("```python\n")
	b.WriteString(strings.Repeat("print('hello world')\n", 50))
	b.WriteString("```\n\n")
	b.WriteString("More text after the code block.\n")
	b.WriteString(strings.Repeat("Another sentence here. ", 100))

	chunks := ChunkText(b.String())
	require.Greater(t, len(chunks), 1)
	for _, chunk := range chunks {
		require.LessOrEqual(t, utf8.RuneCountInString(chunk), MaxChunkSize+10)
	}
}

func TestChunkText_Empty(t *testing.T) {
	chunks := ChunkText("")
	require.Len(t, chunks, 1)
	require.Equal(t, "", chunks[0])
}

func TestChunkText_NoGoodSplitPoint(t *testing.T) {
	// A single very long word
	text := strings.Repeat("x", MaxChunkSize+500)
	chunks := ChunkText(text)
	require.Greater(t, len(chunks), 1)
}

func TestChunkText_WeirdWhitespace(t *testing.T) {
	var b strings.Builder
	// Lots of newlines and spaces
	for i := 0; i < 200; i++ {
		b.WriteString("  \t  word  \n\n\n")
	}
	chunks := ChunkText(b.String())
	for _, chunk := range chunks {
		require.LessOrEqual(t, utf8.RuneCountInString(chunk), MaxChunkSize+10)
	}
}
