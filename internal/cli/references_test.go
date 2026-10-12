package cli

import (
	"testing"

	"github.com/wimcraft/lazyrecall/internal/transcript"
)

// All content below is synthetic, hand-written test data - never real
// session content.

// TestExtractReferencesFilesDeduplicates covers the source of the Files
// list: the paths of the file tools' calls, unioned across turns in
// first-seen order, so a file read twice is still listed once.
func TestExtractReferencesFilesDeduplicates(t *testing.T) {
	turns := []transcript.Turn{
		{Kind: transcript.KindToolUse, Tool: []string{"Read"}, Files: []string{"internal/cli/browse.go", "internal/cli/export.go"}},
		{Kind: transcript.KindToolUse, Tool: []string{"Read", "Edit"}, Files: []string{"internal/cli/export.go", "internal/cli/references.go"}},
	}

	got := ExtractReferences(turns).Files
	want := []string{"internal/cli/browse.go", "internal/cli/export.go", "internal/cli/references.go"}
	if len(got) != len(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractReferencesURLsTrimAndDeduplicate covers the URL rules: the
// sentence's own punctuation is not part of the link, a link in parentheses
// ends at the parenthesis, and the same link said twice in one session is
// listed once.
func TestExtractReferencesURLsTrimAndDeduplicate(t *testing.T) {
	turns := []transcript.Turn{
		{Kind: transcript.KindUserPrompt, Text: "see https://example.com/a, and (https://example.com/b)."},
		{Kind: transcript.KindAssistantText, Text: "as in https://example.com/a"},
	}

	got := ExtractReferences(turns).URLs
	want := []string{"https://example.com/a", "https://example.com/b"}
	if len(got) != len(want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("url %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractReferencesCodeBlocks pins the fenced-block rules: the info
// string becomes Lang, the bare fence has none, a longer fence holds a
// shorter one inside it, an unclosed fence is dropped rather than kept to
// the end of the turn, and a user's own pasted block is not the assistant's
// code.
func TestExtractReferencesCodeBlocks(t *testing.T) {
	turns := []transcript.Turn{
		{Kind: transcript.KindAssistantText, Text: "Here:\n\n```go\nx := 1\n```\n\nthen:\n\n```\nplain text\n```\n"},
		{Kind: transcript.KindAssistantText, Text: "nested:\n\n````\n```\ninner\n```\n````\n"},
		{Kind: transcript.KindAssistantText, Text: "cut off:\n\n```go\nx := 1\n"},
		{Kind: transcript.KindUserPrompt, Text: "mine:\n\n```go\nx := 1\n```\n"},
	}

	got := ExtractReferences(turns).Code
	want := []CodeBlock{
		{Lang: "go", Body: "x := 1"},
		{Lang: "", Body: "plain text"},
		{Lang: "", Body: "```\ninner\n```"},
	}
	if len(got) != len(want) {
		t.Fatalf("code = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestExtractReferencesEmpty covers the empty conversation: nothing to
// point at, so every list stays empty rather than being null-vs-empty
// special-cased by a caller.
func TestExtractReferencesEmpty(t *testing.T) {
	got := ExtractReferences(nil)
	if len(got.Files) != 0 || len(got.URLs) != 0 || len(got.Code) != 0 {
		t.Errorf("empty input produced %+v", got)
	}
}
