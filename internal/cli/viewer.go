// The external viewer: `v` in the browser writes the selected session's
// conversation to a temporary Markdown file and hands it to a program the
// user already reads Markdown with (glow, leaf, bat, nvim, less, ...), then
// removes the file when that program exits. This is the deliberate
// alternative to carrying a rendering library in the binary (roadmap N10,
// revised 2026-10-12): lazyrecall's own code styles Markdown for the
// Transcript tab, and anything richer is left to the tool the user chose.
package cli

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wimcraft/lazyrecall/internal/search"
	"github.com/wimcraft/lazyrecall/internal/transcript"
)

// viewerDoneMsg reports how the external viewer exited. An error is the
// viewer failing to start or exiting non-zero; either way the temporary
// file has already been removed by the callback that produced this message.
type viewerDoneMsg struct{ err error }

// startViewer runs the viewer on the terminal for the duration and turns its
// exit into a message. It is a package variable rather than a plain call so a
// test can record the argv and the callback instead of starting a program:
// the browser's own key test must never depend on a viewer being installed,
// and a test process has no terminal to attach one to anyway.
//
// tea.ExecProcess is what makes "return to the browser when the viewer
// exits" work: bubbletea releases the terminal before the program runs and
// restores the alternate screen and the terminal modes when it returns, so
// an interactive viewer (nvim, less) gets a real terminal and the browser
// comes back with its selection, scroll position and filters intact.
var startViewer = func(argv []string, done func(error) tea.Msg) tea.Cmd {
	return tea.ExecProcess(exec.Command(argv[0], argv[1:]...), done)
}

// viewerArgv is the command that shows one Markdown file, as an argv rather
// than a shell line: nothing here is word-split, quoted or expanded by a
// shell, so a path containing spaces reaches the viewer as exactly one
// argument. {file} in any element is replaced with path, and when no element
// carries it the path is appended - so both ["glow", "-p"] and
// ["nvim", "-R", "{file}", "+1"] end up naming the file exactly once, and
// the second form keeps a view command's own trailing flags (nvim's "+1")
// after the file rather than stranding them.
//
// An empty viewer means "whatever the user already reads Markdown with":
// $PAGER, split on spaces because that variable is a shell line by
// convention and often carries flags ("less -FRX"), else less -R. pager is
// passed in rather than read here so the whole fallback chain is testable
// without touching the environment.
func viewerArgv(viewer []string, file, pager string) []string {
	if len(viewer) > 0 {
		return withViewerFile(viewer, file)
	}
	if fields := strings.Fields(pager); len(fields) > 0 {
		return withViewerFile(fields, file)
	}
	return []string{"less", "-R", file}
}

// withViewerFile substitutes file for every {file} placeholder in argv, or
// appends it when argv has none - the two halves of viewerArgv's rule,
// shared by the $PAGER branch so a pager spelled with a placeholder behaves
// the same way a configured viewer does.
//
// The copy is deliberate: appending to argv itself would write into the
// caller's backing array (the config's slice, or strings.Fields' result) on
// a later call, making the same input render a different argv the second
// time.
func withViewerFile(argv []string, file string) []string {
	out := make([]string, len(argv))
	substituted := false
	for i, a := range argv {
		if strings.Contains(a, "{file}") {
			out[i] = strings.ReplaceAll(a, "{file}", file)
			substituted = true
			continue
		}
		out[i] = a
	}
	if substituted {
		return out
	}
	return append(out, file)
}

// writeViewerFile renders one session's conversation to a fresh temporary
// Markdown file and returns its path. Removing that file is the caller's
// job - it has to outlive this call, because the viewer is what reads it -
// but a failure here removes the half-written file rather than leaving it
// behind in $TMPDIR.
//
// It calls WriteMarkdown with redact false on purpose: the document goes to
// a viewer on the user's own screen, the same trust boundary as the
// Transcript tab `v` sits beside. Redaction exists for the documents that
// leave the machine (the export command's --redact), not for this one, and
// blanking a key the user is reading on their own terminal would be a
// silent lie about what the session contains.
//
// clean carries the browser's active transcript mode, so the file the viewer
// shows is the same question/answer or full technical view the pane was
// showing when the key was pressed.
func writeViewerFile(it search.Item, turns []transcript.Turn, clean bool) (string, error) {
	f, err := os.CreateTemp("", "lazyrecall-*.md")
	if err != nil {
		return "", err
	}
	if err := WriteMarkdown(f, it, turns, clean, false); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// openViewer is the `v` key: it renders the selected session's conversation
// to a temporary Markdown file and hands it to the configured viewer,
// removing the file when the viewer exits. With nothing selected there is
// nothing to show, so it does nothing - the same silence `z` keeps when
// there is no pane to zoom into, rather than a notice about a key that had
// no target.
//
// Both the read and the write can fail for reasons the user can act on - a
// transcript the agent cleaned up since the last refresh, a full $TMPDIR -
// so a failure lands in m.notice, where the refresh errors already go,
// instead of being swallowed by a key that appeared to do nothing at all.
func (m *browseModel) openViewer() (tea.Model, tea.Cmd) {
	it := m.current()
	if it == nil {
		return m, nil
	}
	turns, err := LoadConversation(*it, m.installs(), ExportLimits)
	if err != nil {
		m.notice = "view: " + err.Error()
		return m, nil
	}
	path, err := writeViewerFile(*it, turns, m.transcriptMode == transcriptClean)
	if err != nil {
		m.notice = "view: " + err.Error()
		return m, nil
	}
	return m, startViewer(viewerArgv(m.viewer, path, os.Getenv("PAGER")), func(err error) tea.Msg {
		os.Remove(path)
		return viewerDoneMsg{err}
	})
}
