package cli

import "strings"

// styleMarkdownLines restyles the Markdown an agent's reply is written in -
// headings, bullets, quotes, bold, inline code and italics - inside the
// terminal's own ANSI codes, so the Transcript tab shows the reply the
// agent meant rather than its source.
//
// It is deliberately small. Full Markdown rendering is a dependency (or a
// hand-written parse of the whole grammar) in a program whose point is one
// small static binary, and agent replies are not documents: a paragraph or
// three of prose with a list and a code block in them is what they actually
// look like. The markers handled here cover that; anything richer - tables,
// links, nested emphasis - is left alone and reads as the plain text it
// already was, which is what the external viewer, the one that does render
// Markdown properly, is for.
//
// It works on the *wrapped* lines the transcript is built from rather than
// on the turn's text, for one reason: line numbers. The Transcript tab
// records search hits as rendered line offsets and n/N scroll to those
// offsets, so styling must never add or drop a line - this function maps
// one input line to exactly one output line, always. The cost of that
// choice is that a marker pair the wrapping split across a line break
// ("**bold" on one line, "text**" on the next) is not seen as a pair and
// stays raw; that is the same trade every other choice here makes.
//
// With on false nothing is removed and nothing is styled. The raw Markdown
// already is the plain-text rendering, and an unstyled transcript (a
// redirected one, or NO_COLOR) must come out byte for byte as it always has.
func styleMarkdownLines(lines []string, on bool) []string {
	if !on {
		return lines
	}
	out := make([]string, 0, len(lines))
	inFence := false
	fenceLen := 0
	for _, line := range lines {
		if n, info := markdownFence(line); n >= 3 {
			switch {
			case inFence && n >= fenceLen:
				// The closing fence: a run at least as long as the one
				// that opened the block, which is what the marker
				// requires and what tells a ``` inside a ``` (a shorter
				// run) apart from the real end of the block.
				inFence = false
				out = append(out, style(markdownFenceRule(""), ansiDim, on))
				continue
			case !inFence:
				inFence = true
				fenceLen = n
				out = append(out, style(markdownFenceRule(info), ansiDim, on))
				continue
			}
		}
		if inFence {
			// A code line is not prose: its markers are the code itself,
			// so the line is coloured to mark the block and otherwise
			// left exactly as written. An unclosed fence simply runs to
			// the end of the turn - there is no later line that could
			// close it.
			out = append(out, style(line, ansiCyan, on))
			continue
		}
		out = append(out, markdownLine(line))
	}
	return out
}

// markdownLine restyles one line of prose: a heading, a bullet, a quote, or
// plain text with inline markers. The three block forms are checked first,
// because each of them consumes the character an inline marker would
// otherwise be found on ("* item" is a bullet, "*item*" is emphasis).
func markdownLine(line string) string {
	if text, ok := markdownHeadingText(line); ok {
		// The hashes go and the text is one bold run. Inline markers
		// inside a heading are deliberately left raw: bolding a heading
		// and then bolding part of it again would end the heading's own
		// bold at the inner marker's reset, and the heading would trail
		// off mid-sentence in the terminal.
		return style(text, ansiBold, true)
	}
	if indent, rest, ok := markdownBulletText(line); ok {
		if rest == "" {
			return indent + "•"
		}
		return indent + "• " + markdownInline(rest)
	}
	if rest, ok := markdownQuoteText(line); ok {
		if rest == "" {
			return style("│", ansiDim, true)
		}
		return style("│ "+rest, ansiDim, true)
	}
	return markdownInline(line)
}

// markdownFence reports whether line opens or closes a fenced code block:
// three or more backticks after leading blanks, plus the info string (the
// language name) that may follow them, of which only the first word is
// kept. It returns 0 when the line is not a fence.
func markdownFence(line string) (int, string) {
	trimmed := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(trimmed) && trimmed[n] == '`' {
		n++
	}
	if n < 3 {
		return 0, ""
	}
	rest := strings.Fields(trimmed[n:])
	if len(rest) == 0 {
		return n, ""
	}
	return n, rest[0]
}

// markdownFenceRule is the line a fence is replaced by: a horizontal rule
// naming the language, so a code block is visible as one at a glance, and
// just the rule when the fence named no language.
func markdownFenceRule(info string) string {
	if info == "" {
		return "──"
	}
	return "── " + info + " ──"
}

// markdownHeadingText returns the text of an ATX heading line (one to six
// hashes followed by whitespace), with the hashes and that whitespace
// removed, or ok false when the line is not one. "#hashtag" is a hashtag,
// not a heading, which is why the whitespace is required.
func markdownHeadingText(line string) (text string, ok bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return "", false
	}
	if !markdownSpaceBefore(line, n) {
		return "", false
	}
	i := n
	for i < len(line) && markdownSpace(line[i]) {
		i++
	}
	return line[i:], true
}

// markdownBulletText splits a list item line ("- ", "* " or "+ " after
// optional indentation) into its indentation and the item's text, or ok
// false when the line is not one.
func markdownBulletText(line string) (indent, rest string, ok bool) {
	i := 0
	for i < len(line) && markdownSpace(line[i]) {
		i++
	}
	if i >= len(line) || (line[i] != '-' && line[i] != '*' && line[i] != '+') {
		return "", "", false
	}
	if !markdownSpaceBefore(line, i+1) {
		return "", "", false
	}
	j := i + 1
	for j < len(line) && markdownSpace(line[j]) {
		j++
	}
	return line[:i], line[j:], true
}

// markdownQuoteText returns the text of a block quote line (">" followed by
// at most one space), or ok false when the line is not one.
func markdownQuoteText(line string) (string, bool) {
	if line == "" || line[0] != '>' {
		return "", false
	}
	rest := line[1:]
	if rest != "" && rest[0] == ' ' {
		rest = rest[1:]
	}
	return rest, true
}

// markdownSpaceBefore reports whether line has whitespace at i, which is
// how a marker is told apart from a word that merely starts with one of the
// same characters.
func markdownSpaceBefore(line string, i int) bool {
	return i < len(line) && markdownSpace(line[i])
}

// markdownSpace reports whether c is the whitespace Markdown's block
// markers must be followed by - space or tab, the two a wrapped line can
// actually contain.
func markdownSpace(c byte) bool {
	return c == ' ' || c == '\t'
}

// markdownInline restyles the inline markers in one line: **bold** and
// __bold__, `code`, and single-marker *italic* / _italic_. A marker whose
// pair is missing from the line, or one that sits inside a word, is left
// exactly as it is - snake_case_name and 2*3*4 are names and arithmetic, not
// emphasis, and half-eaten markers are worse than none at all.
//
// Every decision is taken against the raw line, never against the output
// built so far: the emphasis boundary rules ask what character precedes or
// follows a marker, and the answer has to be the one the agent typed, not
// an escape sequence written by an earlier marker on the same line.
func markdownInline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				b.WriteString(ansiCyan + s[i+1:i+1+j] + ansiReset)
				i += j + 2
				continue
			}
		case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "__"):
			marker := s[i : i+2]
			if j := strings.Index(s[i+2:], marker); j > 0 {
				b.WriteString(ansiBold + s[i+2:i+2+j] + ansiReset)
				i += j + 4
				continue
			}
		case (s[i] == '*' || s[i] == '_') && !markdownWordByte(s, i-1):
			if j := strings.IndexByte(s[i+1:], s[i]); j > 0 && !markdownWordByte(s, i+j+2) {
				b.WriteString(ansiItalic + s[i+1:i+1+j] + ansiReset)
				i += j + 2
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// markdownWordByte reports whether s[i] is a character that makes an
// adjacent single marker part of a word rather than emphasis - the check
// behind "snake_case_name and a*b*c stay as they are". An index outside s
// is not a word character, which is what lets emphasis sit at either end of
// a line.
func markdownWordByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
