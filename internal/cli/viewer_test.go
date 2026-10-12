package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// All content below is synthetic, hand-written test data - never real
// session content.

// TestViewerArgv covers the whole command-building rule: a configured argv
// with no {file} gets the path appended, one with {file} gets it substituted
// in place (so a viewer's own trailing flags stay last), and an unconfigured
// viewer falls through $PAGER - split on spaces, since the variable carries
// flags by convention - to less -R.
func TestViewerArgv(t *testing.T) {
	file := filepath.Join(os.TempDir(), "lazyrecall-x.md")
	cases := []struct {
		name   string
		viewer []string
		pager  string
		want   []string
	}{
		{"configured argv appends the file", []string{"glow", "-p"}, "", []string{"glow", "-p", file}},
		{"{file} is substituted in place", []string{"nvim", "-R", "{file}", "+1"}, "", []string{"nvim", "-R", file, "+1"}},
		{"{file} inside a longer argument", []string{"sh-not-a-shell", "--open={file}"}, "", []string{"sh-not-a-shell", "--open=" + file}},
		{"empty viewer falls back to $PAGER, split on spaces", nil, "less -FRX", []string{"less", "-FRX", file}},
		{"empty viewer and empty pager fall back to less -R", nil, "", []string{"less", "-R", file}},
		{"whitespace-only pager is no pager", nil, "   ", []string{"less", "-R", file}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := viewerArgv(c.viewer, file, c.pager)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("viewerArgv(%q, file, %q) = %q, want %q", c.viewer, c.pager, got, c.want)
			}
		})
	}
}

// TestViewerArgvDoesNotMutateItsInput pins the copy in withViewerFile: the
// config's own slice is handed in on every `v` press, so appending the path
// in place would grow it by one file name per press and make the second view
// open the first session's file as well.
func TestViewerArgvDoesNotMutateItsInput(t *testing.T) {
	viewer := []string{"glow", "-p"}
	first := viewerArgv(viewer, "/tmp/one.md", "")
	second := viewerArgv(viewer, "/tmp/two.md", "")

	if !reflect.DeepEqual(viewer, []string{"glow", "-p"}) {
		t.Errorf("viewerArgv mutated its input: %q", viewer)
	}
	if !reflect.DeepEqual(first, []string{"glow", "-p", "/tmp/one.md"}) {
		t.Errorf("first argv = %q", first)
	}
	if !reflect.DeepEqual(second, []string{"glow", "-p", "/tmp/two.md"}) {
		t.Errorf("second argv = %q - the first call's file leaked into it", second)
	}
}

// TestWriteViewerFileMatchesWriteMarkdown pins the file's content to the
// export renderer: the viewer shows exactly the document `lazyrecall export`
// would write for the same session and mode, so the two can never drift into
// two different Markdown shapes.
func TestWriteViewerFileMatchesWriteMarkdown(t *testing.T) {
	it := transcriptItem(writeFixtureTranscript(t))
	turns, err := LoadConversation(it, nil, ExportLimits)
	if err != nil {
		t.Fatal(err)
	}

	path, err := writeViewerFile(it, turns, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })

	if filepath.Dir(path) != filepath.Clean(os.TempDir()) {
		t.Errorf("file landed in %q, want %q", filepath.Dir(path), os.TempDir())
	}
	if !strings.HasPrefix(filepath.Base(path), "lazyrecall-") || !strings.HasSuffix(path, ".md") {
		t.Errorf("file name %q does not match lazyrecall-*.md", filepath.Base(path))
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	if err := WriteMarkdown(&want, it, turns, true, false); err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != want.String() {
		t.Errorf("viewer file differs from WriteMarkdown's output:\nfile:\n%s\nwant:\n%s", onDisk, want.String())
	}
	if !strings.Contains(string(onDisk), "why is the migration failing") {
		t.Errorf("the viewer file does not contain the conversation:\n%s", onDisk)
	}
	// Redaction is off on purpose: this is the user's own screen, not a
	// document leaving the machine.
	if !strings.Contains(string(onDisk), "# ") {
		t.Errorf("the viewer file has no title:\n%s", onDisk)
	}
}

// recordingViewer replaces startViewer with a recorder for one test, so the
// browser's key path can be exercised without a viewer installed and without
// a terminal for one to attach to. It returns the calls slice it records
// into and restores the real startViewer on cleanup.
func recordingViewer(t *testing.T) *[]viewerCall {
	t.Helper()
	calls := &[]viewerCall{}
	saved := startViewer
	startViewer = func(argv []string, done func(error) tea.Msg) tea.Cmd {
		*calls = append(*calls, viewerCall{argv: argv, done: done})
		return nil
	}
	t.Cleanup(func() { startViewer = saved })
	return calls
}

// viewerCall is one recorded startViewer call: the argv it was given and the
// callback a real viewer's exit would have produced.
type viewerCall struct {
	argv []string
	done func(error) tea.Msg
}

// viewerBrowser is a browser holding one claude session whose transcript is
// the shared fixture, with the fixture's conversation text on disk.
func viewerBrowser(t *testing.T) *browseModel {
	t.Helper()
	db := browseTestDB(t)
	seedBrowseSession(t, db, "L0", "claude:p:s0", "p", 1, map[string]any{
		"source": "claude", "cwd": "/work/repo", "git_common_root": "/work/repo",
		"topic": "the migration", "last_activity_at": 1700000000, "dir_exists": 1,
		"transcript_path": writeFixtureTranscript(t),
	})
	return newTestBrowser(db, "claude-personal", BrowserOptions{
		Installs: testInstalls("claude-personal", "claude-work"),
	})
}

// TestViewKeyOpensTheViewerAndCleansUp covers the whole `v` path: one call
// with an argv ending in a real, existing file that holds the conversation,
// and a file that is gone once the viewer's callback runs.
func TestViewKeyOpensTheViewerAndCleansUp(t *testing.T) {
	calls := recordingViewer(t)
	m := viewerBrowser(t)
	if m.current() == nil {
		t.Fatal("the fixture browser has nothing selected")
	}

	nm, cmd := m.Update(keyRunes("v"))
	m = nm.(*browseModel)
	if cmd != nil {
		t.Errorf("v returned a command of its own: %v", cmd)
	}

	if len(*calls) != 1 {
		t.Fatalf("startViewer called %d times, want 1", len(*calls))
	}
	argv := (*calls)[0].argv
	if len(argv) == 0 {
		t.Fatal("startViewer was given an empty argv")
	}
	path := argv[len(argv)-1]
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the viewer file %q is not readable: %v", path, err)
	}
	if !strings.Contains(string(content), "why is the migration failing") {
		t.Errorf("the viewer file does not hold the conversation:\n%s", content)
	}
	if m.notice != "" {
		t.Errorf("a successful view left a notice: %q", m.notice)
	}

	if msg := (*calls)[0].done(nil); msg != (viewerDoneMsg{}) {
		t.Errorf("the viewer callback returned %#v, want viewerDoneMsg{}", msg)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the viewer file %q survived the viewer's exit (stat err = %v)", path, err)
	}
}

// TestViewKeyWithNothingSelectedDoesNothing: `v` needs a target, and the
// empty list is the ordinary state of a browser opened on a filter that
// matches nothing. It must not open a viewer on a zero session.
func TestViewKeyWithNothingSelectedDoesNothing(t *testing.T) {
	calls := recordingViewer(t)
	m := newTestBrowser(browseTestDB(t), "p", BrowserOptions{Installs: testInstalls("claude")})
	if m.current() != nil {
		t.Fatal("the empty browser has a selection")
	}

	nm, cmd := m.Update(keyRunes("v"))
	m = nm.(*browseModel)
	if cmd != nil || len(*calls) != 0 {
		t.Errorf("v with nothing selected called startViewer %d times (cmd %v)", len(*calls), cmd)
	}
	if m.notice != "" {
		t.Errorf("v with nothing selected set a notice: %q", m.notice)
	}
}

// TestViewKeyReportsAMissingTranscript: a session whose transcript the agent
// has cleaned up since the last refresh cannot be shown, and a key that
// silently did nothing would look like a broken key rather than a missing
// file.
func TestViewKeyReportsAMissingTranscript(t *testing.T) {
	calls := recordingViewer(t)
	db := browseTestDB(t)
	seedBrowseSession(t, db, "L0", "claude:p:s0", "p", 1, map[string]any{
		"source": "claude", "cwd": "/work/repo", "last_activity_at": 1700000000, "dir_exists": 1,
		"transcript_path": filepath.Join(t.TempDir(), "gone.jsonl"),
	})
	m := newTestBrowser(db, "claude-personal", BrowserOptions{Installs: testInstalls("claude-personal")})

	nm, cmd := m.Update(keyRunes("v"))
	m = nm.(*browseModel)
	if cmd != nil || len(*calls) != 0 {
		t.Errorf("v on a missing transcript called startViewer %d times (cmd %v)", len(*calls), cmd)
	}
	if !strings.HasPrefix(m.notice, "view: ") {
		t.Errorf("notice = %q, want a view: error", m.notice)
	}
}

// TestViewKeyWorksWhileZoomed: the zoom is where a long transcript is
// actually read, so the key that opens the same conversation in a real
// viewer has to reach the zoomed pane's own key set too.
func TestViewKeyWorksWhileZoomed(t *testing.T) {
	calls := recordingViewer(t)
	m := viewerBrowser(t)
	m = update(t, m, keyRunes("z"))
	if !m.zoomed {
		t.Fatal("z did not zoom")
	}

	nm, _ := m.Update(keyRunes("v"))
	m = nm.(*browseModel)
	if len(*calls) != 1 {
		t.Fatalf("startViewer called %d times while zoomed, want 1", len(*calls))
	}
	os.Remove((*calls)[0].argv[len((*calls)[0].argv)-1])
}

// TestViewerDoneMsgReportsOnlyFailures: a viewer that exited cleanly is the
// expected outcome of pressing v and needs no footer line; one that could
// not start is the failure worth naming.
func TestViewerDoneMsgReportsOnlyFailures(t *testing.T) {
	m := viewerBrowser(t)
	m = update(t, m, viewerDoneMsg{})
	if m.notice != "" {
		t.Errorf("a clean viewer exit set a notice: %q", m.notice)
	}

	m = update(t, m, viewerDoneMsg{err: os.ErrNotExist})
	if !strings.HasPrefix(m.notice, "view: ") {
		t.Errorf("notice = %q, want a view: error", m.notice)
	}
}

// TestViewKeyIsAdvertised pins `v` into the binding table the footer and `?`
// are built from - a key bound in handleBrowseKey but absent here would be
// invisible in both.
func TestViewKeyIsAdvertised(t *testing.T) {
	found := false
	for _, a := range browseActions {
		if a.key != "v" {
			continue
		}
		found = true
		if !a.footer {
			t.Error("the v action is not offered in the footer")
		}
		if !a.showsFor(panelSessions) || !a.showsFor(panelDetail) {
			t.Errorf("the v action does not show for Sessions and Detail: %v", a.panels)
		}
	}
	if !found {
		t.Error("browseActions does not document the v binding, so ? and the footer would not list it")
	}
	if !strings.Contains(viewerBrowser(t).helpView(), "v ") {
		t.Error("the help overlay does not list v")
	}
}

// TestViewerFileFollowsTheTranscriptMode: the file the viewer shows is the
// same rendering the pane was on, so switching to full mode has to change
// what `v` writes - the fixture's tool call is hidden in clean mode and
// present in full mode, which is exactly the difference the two modes draw.
func TestViewerFileFollowsTheTranscriptMode(t *testing.T) {
	calls := recordingViewer(t)
	m := viewerBrowser(t)
	m.tab = tabTranscript

	nm, _ := m.Update(keyRunes("v"))
	m = nm.(*browseModel)
	cleanContent, err := os.ReadFile((*calls)[0].argv[len((*calls)[0].argv)-1])
	if err != nil {
		t.Fatal(err)
	}
	(*calls)[0].done(nil)
	if strings.Contains(string(cleanContent), "tool call: Edit") {
		t.Errorf("the clean-mode viewer file shows the tool call:\n%s", cleanContent)
	}

	m = update(t, m, keyRunes("t"))
	if m.transcriptMode != transcriptFull {
		t.Fatalf("t did not switch to full mode, mode = %v", m.transcriptMode)
	}
	nm, _ = m.Update(keyRunes("v"))
	m = nm.(*browseModel)
	if len(*calls) != 2 {
		t.Fatalf("startViewer called %d times, want 2", len(*calls))
	}
	fullContent, err := os.ReadFile((*calls)[1].argv[len((*calls)[1].argv)-1])
	if err != nil {
		t.Fatal(err)
	}
	(*calls)[1].done(nil)
	if !strings.Contains(string(fullContent), "tool call: Edit") {
		t.Errorf("the full-mode viewer file hides the tool call:\n%s", fullContent)
	}
}
