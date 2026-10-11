// Package doctor produces a read-only health report over the local
// installation: which installs exist, whether the program each one resumes
// with is actually on PATH, how many indexed sessions each has, how many of
// those have lost the transcript file the index points at, and how long
// Claude Code will keep transcripts before deleting them (cleanupPeriodDays).
//
// The motivating case is the last two: Claude Code's cleanupPeriodDays
// defaults to 30, so transcripts older than a month are deleted behind the
// user's back, and every session recorded in the index whose transcript is
// gone can no longer be resumed - with nothing telling the user when it
// happened. doctor says so, once, before it costs them the sessions they
// wanted.
//
// It only reads: it never refreshes the index, writes no file and starts no
// process - running it changes nothing about the machine, so it is always
// safe to run when something is not working.
package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wimcraft/lazyrecall/internal/config"
	"github.com/wimcraft/lazyrecall/internal/profile"
	"github.com/wimcraft/lazyrecall/internal/sqlitex"
)

// defaultCleanupDays is what Claude Code uses when its settings.json says
// nothing about cleanupPeriodDays: transcripts older than this many days are
// deleted. It is stated here rather than derived from the source, because it
// is Claude Code's choice of default, not ours.
const defaultCleanupDays = 30

// Check is the health of one install.
type Check struct {
	// Install is the install's name (profile.Profile.Name), the same value
	// the index stores in sessions.install.
	Install string
	// Source is the agent name this install belongs to ("claude", "pi", ...).
	Source string
	// Root is the install's config root, as discovered.
	Root string
	// Program is the executable resuming a session in this install starts
	// (the first element of the source's resume argv template), or "" when
	// the source has no template configured at all.
	Program string
	// ProgramFound reports whether Program was found on PATH. A Program
	// that is empty is never looked up, so this stays false.
	ProgramFound bool
	// Sessions is how many sessions the index holds for this install.
	Sessions int
	// Gone is how many of those sessions have a transcript file recorded
	// that no longer exists - the count of sessions this install can no
	// longer read.
	Gone int
	// CleanupDays is the source's transcript retention in days. Only Claude
	// Code has one, so it is left zero for every other source.
	CleanupDays int
	// CleanupDefault reports that CleanupDays is not what the source's
	// settings file said, but the built-in default, because the file was
	// missing, unreadable, invalid, or said nothing about cleanupPeriodDays.
	CleanupDefault bool
}

// Report is one doctor run: the index's schema version and one Check per
// discovered install, in the order the installs were given.
type Report struct {
	// SchemaVersion is the version stored in the index's schema_meta table,
	// with no "v" prefix (Render adds it), or "" for an index that records
	// none.
	SchemaVersion string
	Checks        []Check
}

// Run builds the report for installs, in the order given. lookPath is
// exec.LookPath in production and a stub in tests. It reads the index
// through db and the filesystem through os, and writes nothing.
func Run(installs []profile.Profile, cfg config.Config, db *sqlitex.Runner, lookPath func(string) (string, error)) (Report, error) {
	version, err := schemaVersion(db)
	if err != nil {
		return Report{}, err
	}
	rep := Report{SchemaVersion: version, Checks: make([]Check, 0, len(installs))}
	for _, p := range installs {
		source := p.Source()
		c := Check{Install: p.Name, Source: source, Root: p.Root()}
		// A source with no resume template at all is reported as
		// "none configured" rather than as a missing program: those are
		// different problems, and only the second one is about the machine.
		if src, ok := cfg.Sources[source]; ok && len(src.Resume) > 0 {
			c.Program = src.Resume[0]
			if _, err := lookPath(c.Program); err == nil {
				c.ProgramFound = true
			}
		}
		if c.Sessions, c.Gone, err = sessionCounts(db, p.Name); err != nil {
			return Report{}, err
		}
		if source == "claude" {
			c.CleanupDays, c.CleanupDefault = cleanupPeriodDays(c.Root)
		}
		rep.Checks = append(rep.Checks, c)
	}
	return rep, nil
}

// Render writes the report as plain text, without colors, one block per
// install. Every line is unconditional, except the cleanup line, which only
// a source that deletes transcripts has anything to say about.
func Render(w io.Writer, r Report) {
	fmt.Fprintf(w, "index schema: v%s\n", r.SchemaVersion)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%s (%s)  %s\n", c.Source, c.Install, abbreviateHome(c.Root))
		switch {
		case c.Program == "":
			fmt.Fprintln(w, "  resume program: none configured")
		case c.ProgramFound:
			fmt.Fprintf(w, "  resume program: %s (found)\n", c.Program)
		default:
			fmt.Fprintf(w, "  resume program: %s (not on PATH)\n", c.Program)
		}
		fmt.Fprintf(w, "  sessions: %d, transcript gone: %d\n", c.Sessions, c.Gone)
		if c.Source != "claude" {
			continue
		}
		if c.CleanupDefault {
			fmt.Fprintf(w, "  cleanupPeriodDays: %d (default) - Claude Code deletes transcripts older than this; set it higher in settings.json\n", c.CleanupDays)
		} else {
			fmt.Fprintf(w, "  cleanupPeriodDays: %d\n", c.CleanupDays)
		}
	}
}

// schemaVersion reads the index's stored schema version. An index that
// records none reports as an empty version rather than an error: it is a
// fact about this machine worth printing, not a failure of the report.
func schemaVersion(db *sqlitex.Runner) (string, error) {
	var rows []struct {
		Value string `json:"value"`
	}
	if err := db.Query(`SELECT value FROM schema_meta WHERE key = 'schema_version';`, &rows); err != nil {
		return "", fmt.Errorf("doctor: reading schema version: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].Value, nil
}

// sessionCounts counts the sessions indexed for one install, and how many of
// them name a transcript file that is no longer there. The two come from one
// read of the install's rows, so they can never disagree about which
// sessions they describe. install travels to SQL through ParamFile.Ref, the
// same way every other value in this program reaches a query.
func sessionCounts(db *sqlitex.Runner, install string) (sessions, gone int, err error) {
	pf, err := db.WriteParams(map[string]any{"install": install})
	if err != nil {
		return 0, 0, err
	}
	defer pf.Close()

	q := fmt.Sprintf(`SELECT transcript_path FROM sessions WHERE install = %s;`, pf.Ref("install"))
	var rows []struct {
		TranscriptPath *string `json:"transcript_path"`
	}
	if err := db.Query(q, &rows); err != nil {
		return 0, 0, fmt.Errorf("doctor: counting sessions for %s: %w", install, err)
	}
	for _, row := range rows {
		sessions++
		if row.TranscriptPath == nil || *row.TranscriptPath == "" {
			continue
		}
		if _, err := os.Stat(*row.TranscriptPath); errors.Is(err, fs.ErrNotExist) {
			gone++
		}
	}
	return sessions, gone, nil
}

// cleanupPeriodDays reads Claude Code's transcript retention out of root's
// settings.json: a top-level cleanupPeriodDays number, or the built-in
// default when the file is missing, unreadable, not valid JSON, or says
// nothing about it. Only Claude Code has this setting; every other source's
// caller must not call this at all.
func cleanupPeriodDays(root string) (days int, isDefault bool) {
	data, err := os.ReadFile(filepath.Join(root, "settings.json"))
	if err != nil {
		return defaultCleanupDays, true
	}
	var settings struct {
		CleanupPeriodDays *int `json:"cleanupPeriodDays"`
	}
	if err := json.Unmarshal(data, &settings); err != nil || settings.CleanupPeriodDays == nil {
		return defaultCleanupDays, true
	}
	return *settings.CleanupPeriodDays, false
}

// abbreviateHome shows the part of path inside the user's home directory as
// "~", the home directory itself as "~" alone, and returns anything else
// unchanged. It is a display-only transform: nothing reported here is ever
// fed back to a command, so a path that is not abbreviated is still correct.
// internal/cli has its own copy of this for the same reason this one exists -
// neither rendering package imports the other for four lines of path work.
func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return path
	}
	if path == home {
		return "~"
	}
	prefix := strings.TrimRight(home, "/") + "/"
	if strings.HasPrefix(path, prefix) {
		return "~/" + path[len(prefix):]
	}
	return path
}
