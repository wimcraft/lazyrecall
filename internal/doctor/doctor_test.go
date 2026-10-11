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
	}}

	var buf bytes.Buffer
	Render(&buf, rep)
	want := "index schema: v10\n" +
		"claude (claude-personal)  ~/.claude-personal\n" +
		"  resume program: claude (found)\n" +
		"  sessions: 63, transcript gone: 0\n" +
		"  cleanupPeriodDays: 3650\n" +
		"pi (pi)  ~/.pi\n" +
		"  resume program: pi (not on PATH)\n" +
		"  sessions: 110, transcript gone: 2\n"
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
	}}})
	want := "index schema: v10\n" +
		"claude (claude)  ~/.claude\n" +
		"  resume program: claude (found)\n" +
		"  sessions: 5, transcript gone: 5\n" +
		"  cleanupPeriodDays: 30 (default) - Claude Code deletes transcripts older than this; set it higher in settings.json\n"
	if got := buf.String(); got != want {
		t.Errorf("render mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}
