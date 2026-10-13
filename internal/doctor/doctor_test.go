package doctor

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wimcraft/lazyrecall/internal/config"
	"github.com/wimcraft/lazyrecall/internal/profile"
	"github.com/wimcraft/lazyrecall/internal/schema"
	"github.com/wimcraft/lazyrecall/internal/sqlitex"
)

// All fixtures below are synthetic, hand-written data - no real session
// content, no real home directory.

// testDB opens a fresh index in a temp directory, the same way
// internal/search's own testDB does, and skips when the sqlite3 the rest of
// the suite's fixtures still locate is not on PATH.
func testDB(t *testing.T) *sqlitex.Runner {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dir := t.TempDir()
	r := &sqlitex.Runner{BinPath: bin, DBPath: filepath.Join(dir, "lazyrecall.db"), TmpDir: dir}
	if _, err := schema.Open(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// seedSession inserts one sessions row carrying only what doctor reads: the
// install it belongs to and the transcript path the index recorded for it.
func seedSession(t *testing.T, db *sqlitex.Runner, id, install, transcriptPath string) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("sessions",
		[]string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "install", "transcript_path"},
		[]map[string]any{{
			"id": id, "source": "claude", "source_session_id": id, "lineage_id": "lin-" + id,
			"end_state": "completed", "resumable": 1, "install": install, "transcript_path": transcriptPath,
		}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

// claudeInstall is the one-install slice every doctor fixture needs.
func claudeInstall(name, root string) []profile.Profile {
	return []profile.Profile{{Name: name, Roots: map[string]string{"claude": root}}}
}

// seedSnapshot inserts one snapshots row carrying only what doctor reads:
// the session it copies and the path of the copy. The size and mtime
// columns are what the copier itself compares against the agent's file and
// are irrelevant here, so they are left zero.
func seedSnapshot(t *testing.T, db *sqlitex.Runner, sessionID, path string) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("snapshots",
		[]string{"session_id", "path", "source_size", "source_mtime", "copied_at"},
		[]map[string]any{{
			"session_id": sessionID, "path": path,
			"source_size": 0, "source_mtime": 0, "copied_at": 0,
		}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

// TestCleanupPeriodDaysReadsSettings covers the file that motivates doctor:
// a configured retention is reported as written, and an install with no
// settings.json at all falls back to Claude Code's own default of 30 days,
// marked as such so the report can say where the number came from.
func TestCleanupPeriodDaysReadsSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configured := filepath.Join(home, ".claude-personal")
	unconfigured := filepath.Join(home, ".claude")
	for _, dir := range []string{configured, unconfigured} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configured, "settings.json"), []byte(`{"cleanupPeriodDays": 3650}`), 0o644); err != nil {
		t.Fatal(err)
	}

	db := testDB(t)
	installs := []profile.Profile{
		{Name: "claude-personal", Roots: map[string]string{"claude": configured}},
		{Name: "claude", Roots: map[string]string{"claude": unconfigured}},
	}
	rep, err := Run(installs, config.Config{Sources: map[string]config.Source{"claude": {Resume: []string{"claude"}}}}, db, func(string) (string, error) {
		return "/usr/local/bin/claude", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := rep.Checks[0]; got.CleanupDays != 3650 || got.CleanupDefault {
		t.Errorf("configured install: got days=%d default=%v, want 3650/false", got.CleanupDays, got.CleanupDefault)
	}
	if got := rep.Checks[1]; got.CleanupDays != 30 || !got.CleanupDefault {
		t.Errorf("unconfigured install: got days=%d default=%v, want 30/true", got.CleanupDays, got.CleanupDefault)
	}
}

// TestGoneCountsMissingTranscripts covers the number the whole command
// exists for: sessions whose transcript the index still points at but that
// no longer exists on disk.
func TestGoneCountsMissingTranscripts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude-personal")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(home, "present.jsonl")
	if err := os.WriteFile(present, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db := testDB(t)
	seedSession(t, db, "keep", "claude-personal", present)
	seedSession(t, db, "lost", "claude-personal", filepath.Join(home, "gone.jsonl"))
	seedSession(t, db, "other-install", "claude", present)

	rep, err := Run(claudeInstall("claude-personal", root), config.Config{}, db, func(string) (string, error) {
		return "", errors.New("not found")
	})
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Checks[0]
	if got.Sessions != 2 || got.Gone != 1 {
		t.Errorf("got sessions=%d gone=%d, want 2/1", got.Sessions, got.Gone)
	}
}

// TestRunReportsMissingProgram covers the second resume failure doctor
// names: the source's resume program is not installed, or no resume command
// is configured at all - two different facts that must not read the same.
func TestRunReportsMissingProgram(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".pi")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db := testDB(t)

	installs := []profile.Profile{
		{Name: "pi", Roots: map[string]string{"pi": root}},
		{Name: "omp", Roots: map[string]string{"omp": root}},
	}
	cfg := config.Config{Sources: map[string]config.Source{
		"pi":  {Resume: []string{"pi", "--session", "{id}"}},
		"omp": {},
	}}
	rep, err := Run(installs, cfg, db, func(string) (string, error) {
		return "", errors.New("not found")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Checks[0]; got.Program != "pi" || got.ProgramFound {
		t.Errorf("pi: got program=%q found=%v, want pi/false", got.Program, got.ProgramFound)
	}
	if got := rep.Checks[1]; got.Program != "" || got.ProgramFound {
		t.Errorf("omp: got program=%q found=%v, want empty/false", got.Program, got.ProgramFound)
	}

	var buf bytes.Buffer
	Render(&buf, rep)
	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("  resume program: pi (not on PATH)\n")) {
		t.Errorf("expected pi to render as not on PATH, got:\n%s", out)
	}
	if !bytes.Contains(buf.Bytes(), []byte("  resume program: none configured\n")) {
		t.Errorf("expected the template-less source to render as none configured, got:\n%s", out)
	}
}

// TestRenderShape pins the report's exact text, including the home
// abbreviation and the default-cleanup warning Claude installs get, since
// this output is the whole command.
func TestRenderShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rep := Report{SchemaVersion: "10", Checks: []Check{
		{
			Install: "claude-personal", Source: "claude", Root: filepath.Join(home, ".claude-personal"),
			Program: "claude", ProgramFound: true, Sessions: 63, Gone: 0,
			CleanupDays: 3650,
		},
		{
			Install: "pi", Source: "pi", Root: filepath.Join(home, ".pi"),
			Program: "pi", ProgramFound: false, Sessions: 110, Gone: 2,
		},
	}, Retain: config.Retain{Annotated: true}, SnapshotDir: filepath.Join(home, ".lazyrecall", "snapshots")}

	var buf bytes.Buffer
	Render(&buf, rep)
	want := "index schema: v10\n" +
		"claude (claude-personal)  ~/.claude-personal\n" +
		"  resume program: claude (found)\n" +
		"  sessions: 63, transcript gone: 0\n" +
		"  cleanupPeriodDays: 3650\n" +
		"pi (pi)  ~/.pi\n" +
		"  resume program: pi (not on PATH)\n" +
		"  sessions: 110, transcript gone: 2\n" +
		"retain: annotated sessions\n" +
		"snapshots: 0 copies, 0 B in ~/.lazyrecall/snapshots\n"
	if got := buf.String(); got != want {
		t.Errorf("render mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderMarksDefaultCleanup pins the one line that carries a warning:
// an install relying on Claude Code's own default gets told so, in full,
// because that default is what deletes the transcripts doctor just counted
// as gone.
func TestRenderMarksDefaultCleanup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var buf bytes.Buffer
	Render(&buf, Report{SchemaVersion: "10", Checks: []Check{{
		Install: "claude", Source: "claude", Root: filepath.Join(home, ".claude"),
		Program: "claude", ProgramFound: true, Sessions: 5, Gone: 5,
		CleanupDays: 30, CleanupDefault: true,
	}}, Retain: config.Retain{Annotated: true}, SnapshotDir: filepath.Join(home, ".lazyrecall", "snapshots")})
	want := "index schema: v10\n" +
		"claude (claude)  ~/.claude\n" +
		"  resume program: claude (found)\n" +
		"  sessions: 5, transcript gone: 5\n" +
		"  cleanupPeriodDays: 30 (default) - Claude Code deletes transcripts older than this; set it higher in settings.json\n" +
		"retain: annotated sessions\n" +
		"snapshots: 0 copies, 0 B in ~/.lazyrecall/snapshots\n"
	if got := buf.String(); got != want {
		t.Errorf("render mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestKeptCountsReadableCopies covers the number that softens the gone
// count: of the sessions whose transcript is gone, how many lazyrecall can
// still read from its own copy - a gone session with a live snapshot, a
// gone session without one, and a live session that is neither.
func TestKeptCountsReadableCopies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".claude-personal")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(home, "present.jsonl")
	if err := os.WriteFile(present, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(home, "snapshots", "claude-personal", "kept.jsonl")
	if err := os.MkdirAll(filepath.Dir(copyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, []byte("kept copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db := testDB(t)
	seedSession(t, db, "live", "claude-personal", present)
	seedSession(t, db, "kept", "claude-personal", filepath.Join(home, "gone-kept.jsonl"))
	seedSession(t, db, "unkept", "claude-personal", filepath.Join(home, "gone-unkept.jsonl"))
	seedSnapshot(t, db, "kept", copyPath)
	seedSnapshot(t, db, "unkept", filepath.Join(home, "snapshots", "claude-personal", "gone-copy.jsonl"))

	rep, err := Run(claudeInstall("claude-personal", root), config.Config{}, db, func(string) (string, error) {
		return "", errors.New("not found")
	})
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Checks[0]
	if got.Sessions != 3 || got.Gone != 2 || got.Kept != 1 {
		t.Errorf("got sessions=%d gone=%d kept=%d, want 3/2/1", got.Sessions, got.Gone, got.Kept)
	}

	var buf bytes.Buffer
	Render(&buf, rep)
	if !bytes.Contains(buf.Bytes(), []byte("  sessions: 3, transcript gone: 2 (1 readable from lazyrecall's copy)\n")) {
		t.Errorf("expected the kept count on the sessions line, got:\n%s", buf.String())
	}
}

// TestRetainRulesRender pins the retain line for the three shapes a config
// can take: only the built-in rule, every rule at once, and a config that
// keeps nothing. The home directory is set so abbreviateHome is a fixed
// answer rather than whatever the machine's real home is.
func TestRetainRulesRender(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name  string
		rules config.Retain
		want  string
	}{
		{
			name:  "defaults",
			rules: config.Retain{Annotated: true},
			want:  "retain: annotated sessions\n",
		},
		{
			name: "all rules",
			rules: config.Retain{
				Annotated:    true,
				Sources:      []string{"omp", "pi"},
				Paths:        []string{filepath.Join(home, "personal", "*")},
				ExcludePaths: []string{filepath.Join(home, "personal", "scratch", "*")},
			},
			want: "retain: annotated sessions; sources: omp, pi; paths: ~/personal/*; except: ~/personal/scratch/*\n",
		},
		{
			name:  "nothing",
			rules: config.Retain{},
			want:  "retain: nothing (no keep rule)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			Render(&buf, Report{SchemaVersion: "10", Retain: tc.rules, SnapshotDir: filepath.Join(home, ".lazyrecall", "snapshots")})
			if !bytes.Contains(buf.Bytes(), []byte(tc.want)) {
				t.Errorf("render mismatch\n got:\n%s\nwant line:\n%s", buf.String(), tc.want)
			}
		})
	}
}

// TestSnapshotsLine covers the folder summary: two copies that are there,
// one row whose file is gone, and a size that reads in kilobytes rather than
// bytes. The temp dir keeps the whole check inside this worktree.
func TestSnapshotsLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.jsonl"), make([]byte, 536), 0o644); err != nil {
		t.Fatal(err)
	}

	db := testDB(t)
	seedSnapshot(t, db, "a", filepath.Join(dir, "a.jsonl"))
	seedSnapshot(t, db, "b", filepath.Join(dir, "b.jsonl"))
	seedSnapshot(t, db, "gone", filepath.Join(dir, "missing.jsonl"))

	rep, err := Run(nil, config.Config{Retain: config.Retain{Dir: dir}}, db, func(string) (string, error) {
		return "", errors.New("not found")
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Snapshots != 2 || rep.SnapshotMissing != 1 || rep.SnapshotBytes != 1536 {
		t.Errorf("got snapshots=%d missing=%d bytes=%d, want 2/1/1536", rep.Snapshots, rep.SnapshotMissing, rep.SnapshotBytes)
	}

	var buf bytes.Buffer
	Render(&buf, rep)
	want := "snapshots: 2 copies, 1.5 KB in " + abbreviateHome(dir) + ", 1 missing\n"
	if !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Errorf("render mismatch\n got:\n%s\nwant line:\n%s", buf.String(), want)
	}
}

// TestHumanBytes pins the unit boundaries, since the sizes it prints are the
// only thing that makes the snapshot count meaningful next to `du -sh`.
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{3 * 1024 * 1024, "3.0 MB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
