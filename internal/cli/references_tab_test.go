package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureReferencesFence = "```"

// All transcript content below is synthetic, hand-written test data - never
// real session content. It carries one of each thing the References tab
// lists: a file tool call with a path, a link in a prompt, and a fenced
// block in a reply. The tool call names two files so the count in the
// heading is not accidentally right for a list of one.
const fixtureReferencesTranscript = `{"type":"user","message":{"role":"user","content":"look at https://example.com/a"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/repo/internal/app.go","old_string":"a","new_string":"b"}},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/repo/README.md"}}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:02Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed it:\n\n` + fixtureReferencesFence + `go\nfunc main() {}\n` + fixtureReferencesFence + `\n"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:04Z"}
`

func writeReferencesTranscript(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "refs.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReferencesTabRendersTheThreeSections pins the whole page: the headings
// with their counts, the two-space indent, the order Files, Links, Code, and
// the "── go ──" header over the block the reply wrote.
func TestReferencesTabRendersTheThreeSections(t *testing.T) {
	c := &conversationCache{}
	out := renderItemReferences(c, transcriptItem(writeReferencesTranscript(t, fixtureReferencesTranscript)), noProfile, RenderOptions{Width: 80})

	want := `Files (2)
  /repo/internal/app.go
  /repo/README.md

Links (1)
  https://example.com/a

Code (1)
  ── go ──
  func main() {}`
	if out != want {
		t.Errorf("references pane = %q, want %q", out, want)
	}
}

// A conversation with nothing to point at gets one sentence rather than
// three empty headings - and the sentence is dim, like every other "there is
// nothing here" message the pane shows.
func TestReferencesTabSaysWhenThereIsNothingToPointAt(t *testing.T) {
	c := &conversationCache{}
	out := renderItemReferences(c, transcriptItem(writeFixtureTranscript(t)), noProfile, RenderOptions{Width: 80})

	if !strings.Contains(out, "No files, links or code blocks in this conversation.") {
		t.Errorf("expected the empty-references message, got:\n%s", out)
	}
	if strings.Contains(out, "Files (") || strings.Contains(out, "Links (") || strings.Contains(out, "Code (") {
		t.Errorf("an empty list still got a heading:\n%s", out)
	}
}

// The pane reads the same conversation the Transcript tab does, so it has to
// explain the same absences the same way: a transcript the index has a path
// for but that the agent has since cleaned up is "gone", not an error.
func TestReferencesTabExplainsAMissingFile(t *testing.T) {
	c := &conversationCache{}
	gone := filepath.Join(t.TempDir(), "cleaned-up.jsonl")
	out := renderItemReferences(c, transcriptItem(gone), noProfile, RenderOptions{Width: 80})

	if !strings.Contains(out, "cleaned it up") {
		t.Errorf("expected the cleanup explanation, got:\n%s", out)
	}
}

// The pane is rebuilt on every frame, so switching to this tab must reuse the
// Transcript tab's read rather than going back to disk for the same file.
// Deleting the transcript after the first render is how a test can see that
// the second one never opened it.
func TestReferencesTabReusesTheTranscriptRead(t *testing.T) {
	path := writeReferencesTranscript(t, fixtureReferencesTranscript)
	it := transcriptItem(path)
	c := &conversationCache{}

	renderItemTranscript(c, it, "", noProfile, RenderOptions{Width: 80})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out := renderItemReferences(c, it, noProfile, RenderOptions{Width: 80})

	if !strings.Contains(out, "/repo/internal/app.go") {
		t.Errorf("references re-read the deleted file instead of using the cache:\n%s", out)
	}
}

// References sits between the Transcript and the Comments: ] from the
// Transcript reaches it, and one more press leaves it for Comments.
func TestBracketsWalkTranscriptReferencesComments(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "L0", "claude:p:s0", "p", 1, map[string]any{
		"source": "claude", "cwd": "/work/repo", "git_common_root": "/work/repo",
		"topic": "the references", "last_activity_at": 1700000000, "dir_exists": 1,
		"transcript_path": writeReferencesTranscript(t, fixtureReferencesTranscript),
	})
	m := newTestBrowser(db, "claude-personal", BrowserOptions{Installs: testInstalls("claude-personal")})
	m.tab = tabTranscript

	m = update(t, m, keyRunes("]"))
	if m.tab != tabReferences {
		t.Fatalf("] from the Transcript landed on %v, want References", m.tab)
	}
	if got := m.detailContent(RenderOptions{Width: 80}); !strings.Contains(got, "/repo/README.md") {
		t.Errorf("the References tab rendered no files:\n%s", got)
	}
	m = update(t, m, keyRunes("]"))
	if m.tab != tabComments {
		t.Errorf("] from References landed on %v, want Comments", m.tab)
	}
}
