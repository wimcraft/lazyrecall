package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/wimcraft/lazyrecall/internal/profile"
	"github.com/wimcraft/lazyrecall/internal/refresh"
	"github.com/wimcraft/lazyrecall/internal/search"
	"github.com/wimcraft/lazyrecall/internal/session"
	"github.com/wimcraft/lazyrecall/internal/transcript"
)

// ExportLimits is what a whole-conversation export reads with - deliberately
// far larger than the browser's DefaultConversationLimits, whose small
// budgets exist to keep a scrolling pane responsive rather than to be the
// truth about a session. An export is the record itself, so it must not drop
// a turn the file still holds: MaxTurns is high enough to be a runaway
// guard rather than a policy, and MaxTurnBytes keeps only a truly enormous
// paste from being kept whole.
var ExportLimits = transcript.ConversationLimits{MaxTurns: 100000, MaxTurnBytes: 1 << 20}

// LoadConversation reads a session's conversation for export, taking the
// same two paths the browser's Transcript tab does and for the same reason:
// claude, pi and omp keep transcript files that internal/transcript parses,
// while goose, hermes, opencode and kilo keep theirs in the same database
// their adapter already queries. It exists beside conversationCache.load
// rather than reusing it because the cache is a pane's cache - keyed,
// sticky, and bound to the browser's small limits - while an export wants
// one read at export limits with an error it can report.
//
// installs is the discovered installs to resolve a database-backed session's
// own root from (the install named in its composite session id), since one
// index now covers every install's data together. The file-backed sources
// never consult it.
func LoadConversation(it search.Item, installs []profile.Profile, limits transcript.ConversationLimits) ([]transcript.Turn, error) {
	path := ""
	if it.TranscriptPath != nil {
		path = *it.TranscriptPath
	}
	if vocab, ok := transcript.VocabFor(it.Source); ok {
		if path == "" {
			return nil, errors.New("no transcript file is recorded for this session")
		}
		turns, _, err := transcript.Conversation(path, vocab, limits)
		if err != nil {
			return nil, fmt.Errorf("reading transcript: %w", err)
		}
		return turns, nil
	}

	reader, ok := refresh.ConversationReaderFor(it.Source, "")
	if !ok {
		return nil, fmt.Errorf("%s conversations cannot be read by lazyrecall", it.Source)
	}
	name := session.InstallFromID(it.SessionID)
	var install profile.Profile
	found := false
	for _, p := range installs {
		if p.Name == name {
			install, found = p, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no discovered install named %q", name)
	}
	turns, _, err := reader.Conversation(install, it.SourceSessionID, limits)
	if err != nil {
		return nil, fmt.Errorf("reading conversation: %w", err)
	}
	return turns, nil
}

// WriteMarkdown renders a conversation as a Markdown document: a title and
// metadata header, then one section per turn. With clean set the turns pass
// through cleanTranscriptTurns first, so an export can carry the same
// question/answer view the Transcript tab offers; with redact set every
// turn's text and the title are scrubbed of credentials, so the document is
// safe to paste somewhere else.
func WriteMarkdown(w io.Writer, it search.Item, turns []transcript.Turn, clean, redact bool) error {
	if clean {
		turns = cleanTranscriptTurns(turns)
	}

	var b strings.Builder
	title := exportTitle(it)
	if redact {
		title = redactSecrets(title)
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	meta := []string{"- session: " + it.SessionID, "- agent: " + it.Source}
	if it.CWD != nil {
		meta = append(meta, "- cwd: "+*it.CWD)
	}
	if it.StartedAt != nil {
		meta = append(meta, "- started: "+it.StartedAt.Format(time.RFC3339))
	}
	if it.LastActivityAt != nil {
		meta = append(meta, "- last active: "+it.LastActivityAt.Format(time.RFC3339))
	}
	b.WriteString(strings.Join(meta, "\n"))
	b.WriteString("\n\n")

	blocks := make([]string, 0, len(turns))
	for _, t := range turns {
		blocks = append(blocks, markdownTurn(it.Source, t, redact))
	}
	b.WriteString(strings.Join(blocks, "\n\n"))
	b.WriteString("\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// markdownTurn renders one turn as its own Markdown block: the speaker and
// time on the heading line, the body below when it has one. Tool calls and
// compaction boundaries carry no text, so they are a single italic line -
// the same distinction the Transcript tab draws between a turn with a body
// and a turn that is only an event.
func markdownTurn(source string, t transcript.Turn, redact bool) string {
	text := t.Text
	if redact {
		text = redactSecrets(text)
	}

	switch t.Kind {
	case transcript.KindToolUse:
		if len(t.Tool) == 0 {
			return "_tool call_"
		}
		return "_tool call: " + strings.Join(t.Tool, ", ") + "_"
	case transcript.KindCompactionBoundary:
		return "_context compacted_"
	}

	label := source
	if t.Kind == transcript.KindUserPrompt {
		label = "You"
	}
	head := "## " + label
	if t.At != nil {
		head += " · " + t.At.Local().Format("2006-01-02 15:04")
	}
	if text == "" {
		return head
	}
	if t.Truncated {
		text += " …(truncated)"
	}
	return head + "\n\n" + text
}

// exportTitle is the document title, preferring the names a person chose
// (lazyrecall's own custom name, then the source tool's name) over the
// topics and ids that stand in when they did not.
func exportTitle(it search.Item) string {
	for _, candidate := range []*string{it.CustomName, it.Name, it.Topic} {
		if candidate != nil && *candidate != "" {
			return *candidate
		}
	}
	return it.SessionID
}

// exportJSONTurn is one turn's machine-readable shape. Fields are omitted
// when empty rather than emitted as null/zero, matching the session listing
// JSON's own convention; kind is the exception - every turn has one.
type exportJSONTurn struct {
	Kind      string   `json:"kind"`
	Text      string   `json:"text,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	At        string   `json:"at,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// exportJSONDocument is the exported document: the session in the same shape
// `list --json` emits, wrapped around the turns. Reusing jsonItem keeps a
// consumer that already parses the listing from learning a second session
// vocabulary just to read an export.
type exportJSONDocument struct {
	Session jsonItem         `json:"session"`
	Turns   []exportJSONTurn `json:"turns"`
}

// WriteJSON renders a conversation as an indented JSON document. It takes
// the same clean and redact switches as WriteMarkdown, and redaction reaches
// the title-bearing session fields (custom name, name, topic) as well as
// every turn's text, since those are where a title can carry a secret.
func WriteJSON(w io.Writer, it search.Item, turns []transcript.Turn, clean, redact bool) error {
	if clean {
		turns = cleanTranscriptTurns(turns)
	}

	j := toJSONItem(it)
	out := make([]exportJSONTurn, 0, len(turns))
	for _, t := range turns {
		et := exportJSONTurn{Kind: exportKindName(t.Kind), Text: t.Text, Tools: t.Tool, Truncated: t.Truncated}
		if t.At != nil {
			et.At = t.At.Format(time.RFC3339)
		}
		if redact {
			et.Text = redactSecrets(et.Text)
		}
		out = append(out, et)
	}
	if redact {
		j.Name = redactPtr(j.Name)
		j.CustomName = redactPtr(j.CustomName)
		j.Topic = redactPtr(j.Topic)
		j.LastPrompt = redactPtr(j.LastPrompt)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(exportJSONDocument{Session: j, Turns: out})
}

// redactPtr redacts a session's title-bearing field in place when it is set,
// leaving a nil field nil so the JSON keeps omitting it.
func redactPtr(s *string) *string {
	if s == nil {
		return nil
	}
	redacted := redactSecrets(*s)
	return &redacted
}

// exportKindName is the JSON spelling of a turn kind - the values a consumer
// switches on, kept distinct from the internal Kind names so renaming one
// does not silently change the exported contract.
func exportKindName(k transcript.Kind) string {
	switch k {
	case transcript.KindUserPrompt:
		return "user"
	case transcript.KindAssistantText:
		return "assistant"
	case transcript.KindToolUse:
		return "tool_use"
	case transcript.KindCompactionBoundary:
		return "compaction"
	default:
		return "unknown"
	}
}

// redaction is one credential shape plus what replaces it. The value pattern
// keeps the key and separator and drops only the value, so
// `password: hunter22` still reads as a password field with its secret gone;
// the rest replace the whole match.
type redaction struct {
	re   *regexp.Regexp
	repl string
}

// redactions are compiled once at package level: they run over every turn of
// every export, and building the set per call would recompile the same
// expressions for no reason.
//
// The order matters only in that a more specific prefix is listed before a
// broader one, so `sk-ant-…` is recognised as an Anthropic key rather than
// left to the generic `sk-…` rule (which would not match it anyway, but
// keeping the intent explicit stops a future tweak from making the two
// collide).
var redactions = []redaction{
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), "[REDACTED]"},
	{regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`), "[REDACTED]"},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`), "[REDACTED]"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{30,}`), "[REDACTED]"},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "[REDACTED]"},
	{regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{20,}`), "[REDACTED]"},
	// The PEM rule spans lines, hence (?s), and is non-greedy so it removes
	// each block rather than swallowing everything between the first BEGIN
	// and the last END in the document.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED]"},
	// Last, so an assignment whose value is itself a credential is already
	// collapsed to [REDACTED] by the time this runs and simply stays so.
	{regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password|passwd)\b(\s*[:=]\s*)(\S+)`), "$1$2[REDACTED]"},
}

// redactSecrets replaces every credential shape it recognises with
// [REDACTED]. Prose without a secret is returned unchanged.
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
