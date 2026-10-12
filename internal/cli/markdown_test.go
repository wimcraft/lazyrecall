package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fixtures below are synthetic, hand-written test data - never real
// session content.

// TestStyleMarkdownLinesRestylesTheCommonMarkers pins the exact bytes the
// styler emits for each form it handles. Exact strings rather than
// "contains" checks because the point of the styler is what it *removes*:
// a marker left behind in the output is the bug, and a substring assertion
// would not see it.
func TestStyleMarkdownLinesRestylesTheCommonMarkers(t *testing.T) {
	bold := func(s string) string { return ansiBold + s + ansiReset }
	cyan := func(s string) string { return ansiCyan + s + ansiReset }
	dim := func(s string) string { return ansiDim + s + ansiReset }
	italic := func(s string) string { return ansiItalic + s + ansiReset }

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "heading",
			in:   []string{"## Heading text"},
			want: []string{bold("Heading text")},
		},
		{
			name: "bullet with bold and code",
			in:   []string{"- **bold** and `code`"},
			want: []string{"• " + bold("bold") + " and " + cyan("code")},
		},
		{
			name: "nested bullet keeps its indentation",
			in:   []string{"  * item"},
			want: []string{"  • item"},
		},
		{
			name: "quote",
			in:   []string{"> quoted words"},
			want: []string{dim("│ quoted words")},
		},
		{
			name: "italic",
			in:   []string{"an *emphasised* word"},
			want: []string{"an " + italic("emphasised") + " word"},
		},
		{
			// A single marker inside a word is the word itself - a Go
			// identifier and arithmetic, not emphasis - so both lines are
			// returned exactly as they came in.
			name: "word-internal markers stay",
			in:   []string{"snake_case_name and 2*3*4"},
			want: []string{"snake_case_name and 2*3*4"},
		},
		{
			name: "unpaired bold marker stays",
			in:   []string{"a ** b"},
			want: []string{"a ** b"},
		},
		{
			name: "fenced block with a language",
			in:   []string{"```go", "x := **y**", "```"},
			want: []string{dim("── go ──"), cyan("x := **y**"), dim("──")},
		},
		{
			// Nothing closes the fence, so the block simply runs out with
			// the turn.
			name: "unclosed fence",
			in:   []string{"```", "one", "two"},
			want: []string{dim("──"), cyan("one"), cyan("two")},
		},
		{
			// A shorter backtick run inside the block is code, not the
			// end of it: only a run at least as long as the opening one
			// closes.
			name: "shorter run inside a fence is code",
			in:   []string{"````", "``", "````"},
			want: []string{dim("──"), cyan("``"), dim("──")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := styleMarkdownLines(tc.in, true)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines, want %d:\n%q", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("line %d:\n got %q\nwant %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestStyleMarkdownLinesWithStylingOffChangesNothing covers the plain-text
// rendering: with on false the raw Markdown is what is shown, so the
// function must not drop a single byte - a redirected transcript or a
// NO_COLOR terminal is exactly the case tests and users compare against.
func TestStyleMarkdownLinesWithStylingOffChangesNothing(t *testing.T) {
	in := []string{"## Heading", "- **bold** and `code`", "```go", "x := **y**", "```", "> quote"}
	got := styleMarkdownLines(in, false)
	if len(got) != len(in) {
		t.Fatalf("got %d lines, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("line %d changed with styling off:\n got %q\nwant %q", i, got[i], in[i])
		}
	}
}

// TestStyleMarkdownLinesPreservesTheLineCount is the invariant the pane
// depends on: search hits are recorded as rendered line offsets and n/N
// scroll to them, so styling may rewrite a line's bytes but never add or
// remove one - including when a fence is left unclosed, when a marker pair
// is unpaired, or when a line is empty.
func TestStyleMarkdownLinesPreservesTheLineCount(t *testing.T) {
	in := []string{
		"", "# H", "```", "code", "**unpaired", "", "- a", "> b", "text",
	}
	for _, on := range []bool{true, false} {
		got := styleMarkdownLines(in, on)
		if len(got) != len(in) {
			t.Errorf("on=%v: got %d lines, want %d", on, len(got), len(in))
		}
	}
}

// writeMarkdownReplyTranscript writes one assistant turn whose text is
// exactly the Markdown passed in, so a test can drive the Transcript tab
// with a known reply body.
func writeMarkdownReplyTranscript(t *testing.T, text string) string {
	t.Helper()
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` +
		strconv.Quote(text) + `}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:01Z"}` + "\n"
	p := filepath.Join(t.TempDir(), "reply.jsonl")
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTranscriptTabStylesTheAgentsReply covers the tab's half of the
// feature: with styling on and no active phrase, an agent's reply is
// rendered with its Markdown markers gone; with styling off it is rendered
// exactly as the raw transcript has always rendered it.
func TestTranscriptTabStylesTheAgentsReply(t *testing.T) {
	c := &conversationCache{}
	it := transcriptItem(writeMarkdownReplyTranscript(t, "Use **this**"))

	styled := renderItemTranscript(c, it, "", noProfile, RenderOptions{Width: 60, Style: true})
	if strings.Contains(styled, "**") {
		t.Errorf("the reply kept its Markdown markers:\n%s", styled)
	}
	if !strings.Contains(styled, ansiBold+"this"+ansiReset) {
		t.Errorf("expected %q to be bold, got:\n%s", "this", styled)
	}

	plain := renderItemTranscript(&conversationCache{}, transcriptItem(writeMarkdownReplyTranscript(t, "Use **this**")), "", noProfile, RenderOptions{Width: 60})
	if !strings.Contains(plain, "  Use **this**") {
		t.Errorf("with styling off the reply must render as it always has:\n%s", plain)
	}
}

// TestTranscriptTabLeavesRepliesRawWhileSearching covers the other half of
// the rule: an active search phrase turns Markdown styling off for the
// whole tab. Hits and highlighting are computed against the plain text, so
// a styled line would be a line the search can no longer see - and the hit
// recorded for it would point at text that is not there any more.
func TestTranscriptTabLeavesRepliesRawWhileSearching(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, transcriptItem(writeMarkdownReplyTranscript(t, "Use **this**")), "this", noProfile, RenderOptions{Width: 60, Style: true})

	if !strings.Contains(out, "**") {
		t.Errorf("a reply under an active search must stay raw:\n%s", out)
	}
	if strings.Contains(out, ansiBold) {
		t.Errorf("Markdown styling was applied while searching:\n%s", out)
	}
	if len(c.hits) != 1 {
		t.Fatalf("hits = %v, want the one line the phrase matches", c.hits)
	}
	lines := strings.Split(out, "\n")
	if h := c.hits[0]; h >= len(lines) || !strings.Contains(strings.ToLower(lines[h]), "this") {
		t.Errorf("hit line %d does not carry the phrase: %q", h, lines[h])
	}
}
