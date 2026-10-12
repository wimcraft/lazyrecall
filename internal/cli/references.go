package cli

import (
	"regexp"
	"strings"

	"github.com/wimcraft/lazyrecall/internal/transcript"
)

// References is what a conversation points at, each list in first-seen order
// without duplicates.
type References struct {
	// Files are the paths a session's file tools worked on. They come from
	// tool calls rather than from the prose because a tool call is a path
	// the agent actually touched, while a path-shaped word in a reply is
	// usually an example the reader must not be sent to.
	Files []string
	// URLs are the http(s) links a user or the assistant wrote out. A link
	// inside a fenced block counts too: a pasted command or log line is
	// still a place the session points at.
	URLs []string
	// Code is the fenced blocks the assistant wrote. A user's own pasted
	// code is left out deliberately - it is already on the Prompts tab,
	// and repeating it here would list the same block twice.
	Code []CodeBlock
}

// CodeBlock is one fenced code block from an assistant turn.
type CodeBlock struct {
	// Lang is the fence's info string up to the first space - the language
	// tag a renderer would highlight with. Empty when the fence carries
	// none, which is not the same as a fence whose tag is unknown.
	Lang string
	// Body is the lines between the fences joined with "\n", with no
	// trailing newline: the block's own text, not the fence's.
	Body string
}

// urlPattern matches an http(s) URL up to the first character that cannot be
// part of one. Trailing punctuation is trimmed afterwards rather than
// excluded here, because the same characters are legal inside a URL: the
// pattern cannot tell the comma that ends a sentence from the comma in a
// query string, but the end of a match can.
var urlPattern = regexp.MustCompile("https?://[^\\s<>\"'`()\\[\\]{}]+")

// urlTrailingPunctuation is the set stripped from the end of a matched URL.
// Only characters that never end a link: a sentence's full stop or comma, and
// the exclamation or question mark that follows it.
const urlTrailingPunctuation = ".,;:!?"

// ExtractReferences collects the files, links and code blocks a conversation
// points at. It is a pure function over turns rather than over a session, so
// the same list can be built from the browser's small conversation read and
// from an export's unbounded one.
//
// A turn's text may be truncated by the read that produced it
// (Turn.Truncated), so the scan never assumes a fence it opens is closed: an
// unterminated block is dropped instead of swallowing the rest of the turn.
func ExtractReferences(turns []transcript.Turn) References {
	var refs References
	seenFile := map[string]bool{}
	seenURL := map[string]bool{}
	seenCode := map[CodeBlock]bool{}

	for _, turn := range turns {
		for _, path := range turn.Files {
			if path == "" || seenFile[path] {
				continue
			}
			seenFile[path] = true
			refs.Files = append(refs.Files, path)
		}

		if turn.Kind == transcript.KindUserPrompt || turn.Kind == transcript.KindAssistantText {
			for _, match := range urlPattern.FindAllString(turn.Text, -1) {
				url := strings.TrimRight(match, urlTrailingPunctuation)
				if url == "" || seenURL[url] {
					continue
				}
				seenURL[url] = true
				refs.URLs = append(refs.URLs, url)
			}
		}

		if turn.Kind == transcript.KindAssistantText {
			for _, block := range codeBlocks(turn.Text) {
				if seenCode[block] {
					continue
				}
				seenCode[block] = true
				refs.Code = append(refs.Code, block)
			}
		}
	}

	return refs
}

// codeBlocks extracts the fenced code blocks of one turn's text, in order.
// A block with an empty body is left out: a fence with nothing between it
// and its close names no code, and an entry for it would be a blank row.
//
// A block that is never closed is dropped together with everything after it,
// because without a close there is no way to tell where it was meant to end -
// a truncated turn's tail is not code just because a fence opened above it.
func codeBlocks(text string) []CodeBlock {
	lines := strings.Split(text, "\n")
	var out []CodeBlock

	for i := 0; i < len(lines); i++ {
		open := fenceBackticks(lines[i])
		if open < 3 {
			continue
		}

		closeAt := -1
		for j := i + 1; j < len(lines); j++ {
			if run := backtickRun(lines[j]); run >= open {
				closeAt = j
				break
			}
		}
		if closeAt < 0 {
			break
		}

		body := strings.Join(lines[i+1:closeAt], "\n")
		if body != "" {
			out = append(out, CodeBlock{Lang: fenceInfo(lines[i]), Body: body})
		}
		i = closeAt
	}

	return out
}

// fenceBackticks is the length of the leading run of backticks on a fence
// line, or 0 when the line is not one. Three backticks is the shortest fence,
// and only the leading run counts: whatever follows it is the info string.
func fenceBackticks(line string) int {
	trimmed := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(trimmed) && trimmed[n] == '`' {
		n++
	}
	if n < 3 {
		return 0
	}
	return n
}

// backtickRun is the length of the whole trimmed line when it is nothing but
// backticks - a closing fence - and 0 otherwise. Only such a line closes a
// block: a line that starts with backticks but goes on to say something is
// content, which is what keeps a shorter fence nested in a longer one.
func backtickRun(line string) int {
	trimmed := strings.TrimSpace(line)
	n := 0
	for n < len(trimmed) && trimmed[n] == '`' {
		n++
	}
	if n != len(trimmed) {
		return 0
	}
	return n
}

// fenceInfo is the language tag of an opening fence line: the info string up
// to its first space, so that "```go title=main.go" reads as Go.
func fenceInfo(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(trimmed) && trimmed[n] == '`' {
		n++
	}
	info := strings.TrimSpace(trimmed[n:])
	if space := strings.IndexByte(info, ' '); space >= 0 {
		info = info[:space]
	}
	return info
}
