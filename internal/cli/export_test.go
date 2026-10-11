package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wimcraft/lazyrecall/internal/search"
	"github.com/wimcraft/lazyrecall/internal/transcript"
)

// All content below is synthetic, hand-written test data - never real
// session content.

// exportTime is a fixed instant, so the rendered document is byte-stable.
func exportTime(h, m int) *time.Time {
	t := time.Date(2026, 10, 11, h, m, 0, 0, time.UTC)
	return &t
}

// pinUTC makes the local zone the same as the fixed times above, so a
// turn's " · <local time>" is predictable, and restores it on cleanup so a
// test's zone choice cannot leak into another test in this package.
func pinUTC(t *testing.T) {
	t.Helper()
	saved := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = saved })
}

// exportItem is a session whose metadata exercises every header line: it has
// a custom name, a cwd and both timestamps.
func exportItem(customName, cwd string) search.Item {
	started := exportTime(21, 34)
	last := exportTime(21, 40)
	it := search.Item{
		SessionID:      "claude:p:s0",
		Source:         "claude",
		StartedAt:      started,
		LastActivityAt: last,
	}
	if customName != "" {
		it.CustomName = &customName
	}
	if cwd != "" {
		it.CWD = &cwd
	}
	return it
}

// TestWriteMarkdownRendersTheDocument pins the whole Markdown shape: the
// title, the header lines, and one section per turn - user, assistant, tool
// and compaction - with the tool and compaction turns as single italic lines
// because they carry no body.
func TestWriteMarkdownRendersTheDocument(t *testing.T) {
	pinUTC(t)
	turns := []transcript.Turn{
		{Kind: transcript.KindUserPrompt, Text: "why is the migration failing", At: exportTime(21, 34)},
		{Kind: transcript.KindAssistantText, Text: "The migration is missing a down step.", At: exportTime(21, 35)},
		{Kind: transcript.KindToolUse, Tool: []string{"Bash", "Read"}, At: exportTime(21, 36)},
		{Kind: transcript.KindCompactionBoundary, At: exportTime(21, 37)},
	}

	want := `# Deploy fix

- session: claude:p:s0
- agent: claude
- cwd: /work/repo
- started: 2026-10-11T21:34:00Z
- last active: 2026-10-11T21:40:00Z

## You · 2026-10-11 21:34

why is the migration failing

## claude · 2026-10-11 21:35

The migration is missing a down step.

_tool call: Bash, Read_

_context compacted_
`

	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, exportItem("Deploy fix", "/work/repo"), turns, false, false); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != want {
		t.Errorf("markdown mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestWriteMarkdownOmitsWhatItCannotKnow covers the three header rules that
// keep unknown values out of the document: a metadata line is dropped when
// its value is nil, a turn's " · <time>" is dropped when the turn has no
// timestamp, an unnamed tool call reads "_tool call_", and a truncated
// turn's body is marked.
func TestWriteMarkdownOmitsWhatItCannotKnow(t *testing.T) {
	pinUTC(t)
	turns := []transcript.Turn{
		{Kind: transcript.KindUserPrompt, Text: "hello", At: nil},
		{Kind: transcript.KindToolUse, Tool: nil},
		{Kind: transcript.KindAssistantText, Text: "long reply", Truncated: true},
	}

	var buf bytes.Buffer
	// No custom name, no cwd, no timestamps: title falls back to the session
	// id and the cwd/started/last-active lines are gone.
	bare := search.Item{SessionID: "claude:p:s0", Source: "claude"}
	if err := WriteMarkdown(&buf, bare, turns, false, false); err != nil {
		t.Fatal(err)
	}
	got := buf.String()

	if !strings.HasPrefix(got, "# claude:p:s0\n\n- session: claude:p:s0\n- agent: claude\n\n") {
		t.Errorf("expected title fallback and no unknown header lines:\n%s", got)
	}
	for _, gone := range []string{"- cwd:", "- started:", "- last active:"} {
		if strings.Contains(got, gone) {
			t.Errorf("unknown header line %q should be left out:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "## You\n\nhello") {
		t.Errorf("a turn without a time must have no time on its header:\n%s", got)
	}
	if !strings.Contains(got, "_tool call_\n") {
		t.Errorf("an unnamed tool call must read \"_tool call_\":\n%s", got)
	}
	if !strings.Contains(got, "long reply …(truncated)") {
		t.Errorf("a truncated turn must be marked:\n%s", got)
	}
}

// TestWriteMarkdownCleanDropsEventsAndMergesAssistants covers the clean
// switch: tool calls and compaction boundaries disappear, and the two
// assistant replies left adjacent merge into one block under the first
// reply's own timestamp.
func TestWriteMarkdownCleanDropsEventsAndMergesAssistants(t *testing.T) {
	pinUTC(t)
	turns := []transcript.Turn{
		{Kind: transcript.KindUserPrompt, Text: "question", At: exportTime(21, 30)},
		{Kind: transcript.KindAssistantText, Text: "first reply", At: exportTime(21, 31)},
		{Kind: transcript.KindToolUse, Tool: []string{"Bash"}, At: exportTime(21, 32)},
		{Kind: transcript.KindAssistantText, Text: "second reply", At: exportTime(21, 33)},
		{Kind: transcript.KindCompactionBoundary, At: exportTime(21, 34)},
	}

	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, exportItem("", ""), turns, true, false); err != nil {
		t.Fatal(err)
	}
	got := buf.String()

	if strings.Contains(got, "_tool call") || strings.Contains(got, "_context compacted_") {
		t.Errorf("clean mode must hide tool calls and compaction boundaries:\n%s", got)
	}
	wantMerged := "## claude · 2026-10-11 21:31\n\nfirst reply\n\nsecond reply"
	if !strings.Contains(got, wantMerged) {
		t.Errorf("adjacent assistant replies must merge under the first timestamp:\n%s", got)
	}
	if strings.Contains(got, "21:33") {
		t.Errorf("the merged block must not keep the second reply's time:\n%s", got)
	}
}

// TestWriteJSONRoundTrips covers the exported document: it unmarshals back to
// the four turn kinds with their fields intact, under the same session shape
// the listing JSON uses.
func TestWriteJSONRoundTrips(t *testing.T) {
	turns := []transcript.Turn{
		{Kind: transcript.KindUserPrompt, Text: "why", At: exportTime(21, 34)},
		{Kind: transcript.KindAssistantText, Text: "because", Truncated: true},
		{Kind: transcript.KindToolUse, Tool: []string{"Bash", "Read"}},
		{Kind: transcript.KindCompactionBoundary},
	}

	var buf bytes.Buffer
	if err := WriteJSON(&buf, exportItem("Deploy fix", "/work/repo"), turns, false, false); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Session jsonItem `json:"session"`
		Turns   []struct {
			Kind      string   `json:"kind"`
			Text      string   `json:"text"`
			Tools     []string `json:"tools"`
			At        string   `json:"at"`
			Truncated bool     `json:"truncated"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("export is not valid JSON: %v\n%s", err, buf.String())
	}
	if doc.Session.SessionID != "claude:p:s0" || doc.Session.Source != "claude" {
		t.Errorf("session shape lost fields: %+v", doc.Session)
	}
	if len(doc.Turns) != 4 {
		t.Fatalf("want 4 turns, got %d", len(doc.Turns))
	}
	wantKinds := []string{"user", "assistant", "tool_use", "compaction"}
	for i, want := range wantKinds {
		if doc.Turns[i].Kind != want {
			t.Errorf("turn %d kind = %q, want %q", i, doc.Turns[i].Kind, want)
		}
	}
	if doc.Turns[0].At != "2026-10-11T21:34:00Z" {
		t.Errorf("turn at = %q", doc.Turns[0].At)
	}
	if doc.Turns[2].Text != "" || len(doc.Turns[2].Tools) != 2 {
		t.Errorf("tool turn: text=%q tools=%v", doc.Turns[2].Text, doc.Turns[2].Tools)
	}
	if !doc.Turns[1].Truncated {
		t.Error("assistant turn should be marked truncated")
	}
	if doc.Turns[3].Text != "" || doc.Turns[3].At != "" {
		t.Errorf("compaction turn must carry only its kind: %+v", doc.Turns[3])
	}
}

// TestRedactionReplacesCredentials feeds each recognised credential shape
// into a rendered document and checks only the secret is gone; a sentence
// without one is untouched, and turning redaction off changes nothing.
func TestRedactionReplacesCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"anthropic key", "key sk-ant-abcdefghijklmnopqrstuvwxyz here", "key [REDACTED] here"},
		{"openai key", "key sk-abcdefghijklmnopqrstuvwx here", "key [REDACTED] here"},
		{"github token", "key ghp_abcdefghijklmnopqrstuvwxyz0123456789 here", "key [REDACTED] here"},
		{"github refresh", "key ghr_abcdefghijklmnopqrstuvwxyz0123456789 here", "key [REDACTED] here"},
		{"github pat", "key github_pat_abcdefghijklmnopqrstuvwxyz0123 here", "key [REDACTED] here"},
		{"aws key", "key AKIAIOSFODNN7EXAMPLE here", "key [REDACTED] here"},
		{"slack token", "key xoxb-1234567890-abcdef here", "key [REDACTED] here"},
		{"bearer", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz01", "Authorization: [REDACTED]"},
		{"pem block", "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----\nafter", "before\n[REDACTED]\nafter"},
		{"password", "password: hunter22", "password: [REDACTED]"},
		{"api key assignment", "api_key=abc123", "api_key=[REDACTED]"},
		{"prose untouched", "the migration is missing a down step", "the migration is missing a down step"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			turns := []transcript.Turn{{Kind: transcript.KindUserPrompt, Text: c.in}}

			var buf bytes.Buffer
			if err := WriteMarkdown(&buf, exportItem("", ""), turns, false, true); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); !strings.Contains(got, c.want) {
				t.Errorf("redacted output missing %q:\n%s", c.want, got)
			}
			if c.in != c.want && strings.Contains(buf.String(), c.in) {
				t.Errorf("secret %q survived redaction:\n%s", c.in, buf.String())
			}

			// redact == false leaves the text exactly as it was.
			buf.Reset()
			if err := WriteMarkdown(&buf, exportItem("", ""), turns, false, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), c.in) {
				t.Errorf("redact=false must change nothing:\n%s", buf.String())
			}
		})
	}
}

// TestRedactionReachesTheTitleAndSessionFields covers redaction outside the
// turn bodies: a title (and the JSON session name/topic) can carry a secret
// too, so WriteMarkdown and WriteJSON must scrub them as well.
func TestRedactionReachesTheTitleAndSessionFields(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwx token"
	it := exportItem(secret, "")

	var md bytes.Buffer
	if err := WriteMarkdown(&md, it, nil, false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(md.String(), "# [REDACTED] token\n") {
		t.Errorf("markdown title not redacted:\n%s", md.String())
	}
	if strings.Contains(md.String(), "abcdefghijklmnopqrstuvwx") {
		t.Errorf("secret survived in the markdown title:\n%s", md.String())
	}

	var js bytes.Buffer
	if err := WriteJSON(&js, it, nil, false, true); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Session struct {
			CustomName string `json:"custom_name"`
		} `json:"session"`
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Session.CustomName != "[REDACTED] token" {
		t.Errorf("json custom_name = %q, want redacted", doc.Session.CustomName)
	}
}

// TestLoadConversationReadsTheFixtureTranscript covers the file-backed path:
// the fixture's four displayable turns come back in order, through the same
// reader the Transcript tab uses.
func TestLoadConversationReadsTheFixtureTranscript(t *testing.T) {
	turns, err := LoadConversation(transcriptItem(writeFixtureTranscript(t)), nil, ExportLimits)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []transcript.Kind{
		transcript.KindUserPrompt,
		transcript.KindAssistantText,
		transcript.KindToolUse,
		transcript.KindAssistantText,
	}
	if len(turns) != len(wantKinds) {
		t.Fatalf("want %d turns, got %d: %+v", len(wantKinds), len(turns), turns)
	}
	for i, want := range wantKinds {
		if turns[i].Kind != want {
			t.Errorf("turn %d kind = %v, want %v", i, turns[i].Kind, want)
		}
	}
	if turns[0].Text != "why is the migration failing" {
		t.Errorf("user turn text = %q", turns[0].Text)
	}
}

// TestLoadConversationErrors covers the three failures a caller has to be
// able to tell apart: a file-backed session with no recorded path, a source
// whose conversations cannot be read at all, and a database-backed session
// whose install is not among those discovered.
func TestLoadConversationErrors(t *testing.T) {
	if _, err := LoadConversation(search.Item{SessionID: "claude:p:s0", Source: "claude"}, nil, ExportLimits); err == nil || !strings.Contains(err.Error(), "no transcript file is recorded") {
		t.Errorf("missing path error = %v", err)
	}
	if _, err := LoadConversation(search.Item{SessionID: "antigravity:p:s0", Source: "antigravity"}, nil, ExportLimits); err == nil || !strings.Contains(err.Error(), "cannot be read by lazyrecall") {
		t.Errorf("unreadable source error = %v", err)
	}
	it := search.Item{SessionID: "goose:p:s1", Source: "goose", SourceSessionID: "s1"}
	if _, err := LoadConversation(it, nil, ExportLimits); err == nil || !strings.Contains(err.Error(), `no discovered install named "p"`) {
		t.Errorf("missing install error = %v", err)
	}
}
