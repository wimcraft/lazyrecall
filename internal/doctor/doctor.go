// Package doctor produces a read-only health report over the local
// installation: which installs exist, whether the program each one resumes
// with is actually on PATH, how many indexed sessions each has, how many of
// those have lost the transcript file the index points at, and how long
// Claude Code will keep transcripts before deleting them (cleanupPeriodDays).
// It also prints the retain rules that decide which transcripts lazyrecall
// copies into its own folder, and that folder's size - so a "transcript
// gone" count reads together with the copies that survived it.
//
// The motivating case is the deletion itself: Claude Code's cleanupPeriodDays
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
	"github.com/wimcraft/lazyrecall/internal/retain"
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
	// Kept is how many of the Gone sessions still have a readable copy in
	// lazyrecall's own snapshot folder, by a snapshots row whose path
	// exists. It is the part of Gone that is not a loss, which is why the
	// sessions line reports it beside the gone count.
	Kept int
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
	// Retain is the configured keep rules, printed as they are so the
	// snapshot counts read together with the rules that produced them.
	Retain config.Retain
	// SnapshotDir is the folder lazyrecall's copies live in, resolved by
	// retain.DirFor - the configured retain.dir, or the default under
	// lazyrecall's own data directory.
	SnapshotDir string
	// Snapshots is how many snapshots rows name a file that is still there.
	// SnapshotMissing is the rest: rows whose copy is gone too, a record
	// with nothing behind it. SnapshotBytes is the existing copies' total
	// size, as the filesystem reports it.
	Snapshots       int
	SnapshotMissing int
	SnapshotBytes   int64
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
	rep.Retain = cfg.Retain
	rep.SnapshotDir = retain.DirFor(cfg.Retain)
	if rep.Snapshots, rep.SnapshotMissing, rep.SnapshotBytes, err = snapshotStats(db); err != nil {
		return Report{}, err
	}
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
		if c.Sessions, c.Gone, c.Kept, err = sessionCounts(db, p.Name); err != nil {
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
// install, then the retain rules and the snapshot folder's size as two
// lines of their own. Every line is unconditional, except the cleanup line,
// which only a source that deletes transcripts has anything to say about,
// and the parenthesised readable-from-a-copy count, which is only printed
// when there is one.
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
		fmt.Fprintf(w, "  sessions: %d, transcript gone: %d", c.Sessions, c.Gone)
		if c.Kept > 0 {
			fmt.Fprintf(w, " (%d readable from lazyrecall's copy)", c.Kept)
		}
		fmt.Fprintln(w)
		if c.Source != "claude" {
			continue
		}
		if c.CleanupDefault {
			fmt.Fprintf(w, "  cleanupPeriodDays: %d (default) - Claude Code deletes transcripts older than this; set it higher in settings.json\n", c.CleanupDays)
		} else {
			fmt.Fprintf(w, "  cleanupPeriodDays: %d\n", c.CleanupDays)
		}
	}
	fmt.Fprintf(w, "retain: %s\n", retainRules(r.Retain))
	fmt.Fprintf(w, "snapshots: %s, %s in %s", copies(r.Snapshots), humanBytes(r.SnapshotBytes), abbreviateHome(r.SnapshotDir))
	if r.SnapshotMissing > 0 {
		fmt.Fprintf(w, ", %d missing", r.SnapshotMissing)
	}
	fmt.Fprintln(w)
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

// sessionCounts counts the sessions indexed for one install, how many of
// them name a transcript file that is no longer there, and how many of
// those gone sessions still have a readable copy in the snapshot folder. All
// three come from one read of the install's rows, so they can never
// disagree about which sessions they describe; the snapshot path rides
// along on a LEFT JOIN so the gone count and the kept count are decided
// together. install travels to SQL through ParamFile.Ref, the same way every
// other value in this program reaches a query.
func sessionCounts(db *sqlitex.Runner, install string) (sessions, gone, kept int, err error) {
	pf, err := db.WriteParams(map[string]any{"install": install})
	if err != nil {
		return 0, 0, 0, err
	}
	defer pf.Close()

	q := fmt.Sprintf(`SELECT s.transcript_path, sn.path AS snapshot_path FROM sessions s LEFT JOIN snapshots sn ON sn.session_id = s.id WHERE s.install = %s;`, pf.Ref("install"))
	var rows []struct {
		TranscriptPath *string `json:"transcript_path"`
		SnapshotPath   *string `json:"snapshot_path"`
	}
	if err := db.Query(q, &rows); err != nil {
		return 0, 0, 0, fmt.Errorf("doctor: counting sessions for %s: %w", install, err)
	}
	for _, row := range rows {
		sessions++
		if row.TranscriptPath == nil || *row.TranscriptPath == "" {
			continue
		}
		if _, err := os.Stat(*row.TranscriptPath); errors.Is(err, fs.ErrNotExist) {
			gone++
			if row.SnapshotPath != nil && *row.SnapshotPath != "" {
				if _, err := os.Stat(*row.SnapshotPath); err == nil {
					kept++
				}
			}
		}
	}
	return sessions, gone, kept, nil
}

// snapshotStats counts the rows in the durable snapshots table and adds up
// the size of the copies that are still on disk, from one read of the table
// and one Stat per row. A row whose file cannot be stat'ed at all counts as
// missing rather than failing the report: a copy lazyrecall can no longer
// read is exactly the fact worth printing, and a health report should not
// stop because one file went away between the query and the Stat.
func snapshotStats(db *sqlitex.Runner) (existing, missing int, bytes int64, err error) {
	var rows []struct {
		Path string `json:"path"`
	}
	if err := db.Query(`SELECT path FROM snapshots;`, &rows); err != nil {
		return 0, 0, 0, fmt.Errorf("doctor: reading snapshots: %w", err)
	}
	for _, row := range rows {
		info, err := os.Stat(row.Path)
		if err != nil {
			missing++
			continue
		}
		existing++
		bytes += info.Size()
	}
	return existing, missing, bytes, nil
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

// retainRules renders the effective keep rules as one line's worth of text,
// in the order the rules are decided: whether annotated sessions are kept,
// then the named sources, then the cwd patterns, then the patterns that
// never keep anything. Only the parts that apply are printed, so the line
// says what is actually configured rather than reciting every field; a
// config with no keep rule at all gets a sentence saying so, since "retain:"
// with nothing after it reads like a rendering bug rather than a decision.
func retainRules(rules config.Retain) string {
	// No keep rule at all is its own answer rather than an empty list: a
	// lone exclude_paths rule excludes from nothing and keeps nothing, so
	// it is not a rule this line can describe.
	if !rules.Annotated && len(rules.Sources) == 0 && len(rules.Paths) == 0 {
		return "nothing (no keep rule)"
	}
	parts := make([]string, 0, 4)
	if rules.Annotated {
		parts = append(parts, "annotated sessions")
	}
	if len(rules.Sources) > 0 {
		parts = append(parts, "sources: "+strings.Join(rules.Sources, ", "))
	}
	if len(rules.Paths) > 0 {
		parts = append(parts, "paths: "+joinPaths(rules.Paths))
	}
	if len(rules.ExcludePaths) > 0 {
		parts = append(parts, "except: "+joinPaths(rules.ExcludePaths))
	}
	return strings.Join(parts, "; ")
}

// joinPaths abbreviates each path for display and joins them, the same way
// every other path this report prints goes through abbreviateHome - a rule
// the user wrote as ~/personal/** should read back as a home-relative path,
// not as their account name.
func joinPaths(paths []string) string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = abbreviateHome(p)
	}
	return strings.Join(out, ", ")
}

// copies renders a snapshot count with the singular only where it is true:
// "1 copy" reads correctly, "1 copies" does not.
func copies(n int) string {
	if n == 1 {
		return "1 copy"
	}
	return fmt.Sprintf("%d copies", n)
}

// humanBytes renders a byte count at the largest unit that leaves it below
// 1024, with one decimal place above a kilobyte: the snapshot folder is read
// by eye next to `du -sh`, so "1.5 KB" is more use than "1536 B". Steps are
// 1024, matching the filesystem's own units rather than the decimal ones a
// disk vendor would print.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}
