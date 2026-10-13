package refresh

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wimcraft/lazyrecall/internal/adapter"
	"github.com/wimcraft/lazyrecall/internal/profile"
)

// All fixtures below are synthetic, hand-written test data - no real
// session content anywhere in this file.

// snapshotFixture is one refreshed test index with a Claude session the
// tests are about to delete the transcript of, plus the values that rebuild
// must reproduce.
type snapshotFixture struct {
	r            *Refresher
	original     string // the agent's own transcript path (the one that gets deleted)
	copyPath     string // LazyRecall's snapshot copy
	id           string // the session's composite id
	cwd          string
	topic        string
	messageCount int64
}

// newSnapshotFixture refreshes the standard test profile once, copies the
// claude session's transcript into a temp snapshots folder, records the copy
// in the durable snapshots table (with the agent's original path as
// source_path, the way internal/retain's upsertSnapshot writes it), and
// returns everything the rebuild tests need. The original is left on disk;
// the caller decides whether to delete it.
func newSnapshotFixture(t *testing.T) snapshotFixture {
	t.Helper()
	p, bin := buildTestProfile(t)
	r, err := New([]profile.Profile{p}, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		ID           string `json:"id"`
		Transcript   string `json:"transcript_path"`
		CWD          string `json:"cwd"`
		Topic        string `json:"topic"`
		MessageCount int64  `json:"message_count"`
	}
	if err := r.DB.Query(`SELECT id, transcript_path, cwd, topic, message_count FROM sessions WHERE source = 'claude';`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d claude session rows, want 1", len(rows))
	}

	original := rows[0].Transcript
	bytes, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "snapshots", "claude", "claude-personal", "c1.jsonl")
	writeFile(t, copyPath, string(bytes))
	insertSnapshot(t, r, rows[0].ID, copyPath, original)

	return snapshotFixture{r: r, original: original, copyPath: copyPath, id: rows[0].ID, cwd: rows[0].CWD, topic: rows[0].Topic, messageCount: rows[0].MessageCount}
}

// insertSnapshot records one row in the snapshots table, exactly the values
// internal/retain writes: the copy's path, the agent's original path, and
// integer sizes/times this test does not care about.
func insertSnapshot(t *testing.T, r *Refresher, sessionID, path, sourcePath string) {
	t.Helper()
	q := `INSERT INTO snapshots (session_id, path, source_size, source_mtime, copied_at, source_path) VALUES (` +
		quoteStringLiteral(sessionID) + `, ` + quoteStringLiteral(path) + `, 10, 20, 30, ` + quoteStringLiteral(sourcePath) + `);`
	if err := r.DB.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// claudeRowCount reports how many claude session rows the index holds.
func claudeRowCount(t *testing.T, r *Refresher) int {
	t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	if err := r.DB.Query(`SELECT id FROM sessions WHERE source = 'claude';`, &rows); err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// TestFullRebuildReindexesFromSnapshotWhenTranscriptGone is the whole point
// of T27: after the agent's transcript is deleted, a full rebuild used to
// drop the session even though LazyRecall kept a copy. With source_path on
// file (schema v13) the rebuild reads the copy back, storing the agent's
// original path so the session looks exactly like any other snapshotted one.
func TestFullRebuildReindexesFromSnapshotWhenTranscriptGone(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := os.Remove(f.original); err != nil {
		t.Fatal(err)
	}

	if _, err := f.r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		ID           string `json:"id"`
		Transcript   string `json:"transcript_path"`
		CWD          string `json:"cwd"`
		Topic        string `json:"topic"`
		MessageCount int64  `json:"message_count"`
	}
	if err := f.r.DB.Query(`SELECT id, transcript_path, cwd, topic, message_count FROM sessions WHERE source = 'claude';`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d claude session rows after the rebuild, want 1 re-indexed from the snapshot", len(rows))
	}
	got := rows[0]
	if got.Transcript != f.original {
		t.Errorf("transcript_path = %q, want the agent's original path %q", got.Transcript, f.original)
	}
	if got.CWD != f.cwd {
		t.Errorf("cwd = %q, want %q", got.CWD, f.cwd)
	}
	if got.Topic != f.topic {
		t.Errorf("topic = %q, want %q", got.Topic, f.topic)
	}
	if got.MessageCount != f.messageCount {
		t.Errorf("message_count = %d, want %d", got.MessageCount, f.messageCount)
	}

	// Tier 2 must fall back to the copy's transcript-extracted prompt.
	var hits []struct {
		SessionID string `json:"session_id"`
	}
	if err := f.r.DB.Query(`SELECT session_id FROM prompt_fts WHERE prompt_fts MATCH 'flaky';`, &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the re-indexed session's prompt to be searchable, got %d hits", len(hits))
	}
}

// TestFullRebuildDropsSnapshotWhoseCopyIsGone: with both the agent's file
// and the copy gone there is nothing left to read, so the session stays gone.
func TestFullRebuildDropsSnapshotWhoseCopyIsGone(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := os.Remove(f.original); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.copyPath); err != nil {
		t.Fatal(err)
	}

	if _, err := f.r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}
	if n := claudeRowCount(t, f.r); n != 0 {
		t.Fatalf("got %d claude session rows, want 0 - both the transcript and its copy are gone", n)
	}
}

// TestSnapshotDoesNotShadowAPresentTranscript: when the agent's file is
// still there the adapter discovers it, and the snapshot must not add a
// second row for the same session.
func TestSnapshotDoesNotShadowAPresentTranscript(t *testing.T) {
	f := newSnapshotFixture(t)
	if _, err := f.r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}
	if n := claudeRowCount(t, f.r); n != 1 {
		t.Fatalf("got %d claude session rows, want exactly 1 - a present transcript must not be shadowed by its snapshot", n)
	}
	var rows []struct {
		Transcript string `json:"transcript_path"`
	}
	if err := f.r.DB.Query(`SELECT transcript_path FROM sessions WHERE source = 'claude';`, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].Transcript != f.original {
		t.Errorf("transcript_path = %q, want the original %q", rows[0].Transcript, f.original)
	}
}

// TestSnapshotWithoutSourcePathIsIgnored: a row whose source_path is NULL
// (a session already gone before schema v13) has no path to re-index under,
// so it is skipped without error.
func TestSnapshotWithoutSourcePathIsIgnored(t *testing.T) {
	f := newSnapshotFixture(t)
	ghost := filepath.Join(t.TempDir(), "snapshots", "ghost.jsonl")
	writeFile(t, ghost, "{}\n")
	if err := f.r.DB.Exec(`INSERT INTO snapshots (session_id, path, source_size, source_mtime, copied_at, source_path) VALUES (` +
		quoteStringLiteral("claude:claude-personal:ghost") + `, ` + quoteStringLiteral(ghost) + `, 3, 1, 1, NULL);`); err != nil {
		t.Fatal(err)
	}

	if _, err := f.r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatalf("a NULL source_path must be ignored, not fail the refresh: %v", err)
	}
	if n := claudeRowCount(t, f.r); n != 1 {
		t.Fatalf("got %d claude session rows, want 1 - the NULL-source_path snapshot contributes nothing", n)
	}
}

// TestIncrementalRefreshAfterSnapshotReindexDoesNotDoubleCount: the cursor
// written from the copy must be reused by the next pass, so the message
// count does not grow.
func TestIncrementalRefreshAfterSnapshotReindexDoesNotDoubleCount(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := os.Remove(f.original); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		MessageCount int64 `json:"message_count"`
	}
	if err := f.r.DB.Query(`SELECT message_count FROM sessions WHERE source = 'claude';`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MessageCount != f.messageCount {
		t.Fatalf("message_count after a second incremental pass = %+v, want %d - a growing count means the copy was re-scanned from the start", rows, f.messageCount)
	}
}

// TestSnapshotDiscoveredUnit checks snapshotDiscovered's own filters: only
// rows for this (source, install), whose original is gone and whose copy is
// readable, become entries.
func TestSnapshotDiscoveredUnit(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.jsonl")
	writeFile(t, present, "{}\n")
	copyExists := filepath.Join(dir, "copy.jsonl")
	writeFile(t, copyExists, "{\"a\":1}\n")

	gone := filepath.Join(dir, "gone.jsonl")
	copyGone := filepath.Join(dir, "copy-gone.jsonl")

	snaps := []snapshotRow{
		{SessionID: "claude:claude-personal:gone", Path: copyExists, SourcePath: gone},
		{SessionID: "claude:other-install:gone2", Path: copyExists, SourcePath: gone},
		{SessionID: "pi:claude-personal:gone3", Path: copyExists, SourcePath: gone},
		{SessionID: "claude:claude-personal:present", Path: copyExists, SourcePath: present},
		{SessionID: "claude:claude-personal:copygone", Path: copyGone, SourcePath: gone},
	}

	got := snapshotDiscovered("claude", "claude-personal", snaps, nil)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want exactly 1 (only the gone original with a readable copy)", len(got))
	}
	if got[0].Session.SourceSessionID != "gone" {
		t.Errorf("source session id = %q, want %q", got[0].Session.SourceSessionID, "gone")
	}
	if got[0].Session.ID != "claude:claude-personal:gone" || got[0].Session.Source != "claude" || got[0].Session.Profile != "claude-personal" {
		t.Errorf("session = %+v, want the adapters' own shape for claude/claude-personal/gone", got[0].Session)
	}
	if got[0].TranscriptPath != gone {
		t.Errorf("TranscriptPath = %q, want the agent's original path %q", got[0].TranscriptPath, gone)
	}
	if got[0].ReadFrom != copyExists {
		t.Errorf("ReadFrom = %q, want the copy %q", got[0].ReadFrom, copyExists)
	}
	if got[0].FileSize != int64(len("{\"a\":1}\n")) {
		t.Errorf("FileSize = %d, want the copy's size", got[0].FileSize)
	}

	// A transcript the adapter already discovered must never be shadowed.
	discovered := []adapter.Discovered{{TranscriptPath: gone}}
	if got := snapshotDiscovered("claude", "claude-personal", snaps, discovered); len(got) != 0 {
		t.Fatalf("got %d entries, want 0 - the original is already discovered by the adapter", len(got))
	}
}
