package retain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wimcraft/lazyrecall/internal/config"
	"github.com/wimcraft/lazyrecall/internal/schema"
	"github.com/wimcraft/lazyrecall/internal/sqlitex"
)

// testDB opens a temp index at the current schema, the way the annotate
// package's own tests do - copied here rather than shared, since a helper
// cannot cross package boundaries.
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

// session is one row of the disposable index, built by a test to describe
// the session it wants the Snapshot pass to consider. Every field the pass
// reads is here; the rest of the sessions table is left at its defaults.
type session struct {
	id             string
	source         string
	install        string
	sourceID       string
	lineage        string
	cwd            string
	transcriptPath string
}

func seedSession(t *testing.T, db *sqlitex.Runner, s session) {
	t.Helper()
	rec := map[string]any{
		"id": s.id, "source": s.source, "source_session_id": s.sourceID,
		"lineage_id": s.lineage, "end_state": "completed", "resumable": 1,
	}
	if s.install != "" {
		rec["install"] = s.install
	}
	if s.cwd != "" {
		rec["cwd"] = s.cwd
	}
	if s.transcriptPath != "" {
		rec["transcript_path"] = s.transcriptPath
	}
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned"}, []map[string]any{
		{"id": s.lineage, "profile": "p", "orphaned": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkInsert("sessions", keys(rec), []map[string]any{rec}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

// keys returns rec's column names, so a record that carries only the
// columns a test set still names them all explicitly.
func keys(rec map[string]any) []string {
	cols := make([]string, 0, len(rec))
	for k := range rec {
		cols = append(cols, k)
	}
	return cols
}

// annotate applies one annotation to a lineage directly, the way a test
// needs it to be already there before a pass runs. Every value is a literal
// the test itself chose, so building the statement by hand carries no
// injection risk.
func annotate(t *testing.T, db *sqlitex.Runner, lineageID, kind string) {
	t.Helper()
	var sql string
	switch kind {
	case "star":
		sql = `UPDATE lineages SET starred_at = 1700000000 WHERE id = '` + lineageID + `';`
	case "name":
		sql = `UPDATE lineages SET custom_name = 'mine' WHERE id = '` + lineageID + `';`
	case "tag":
		sql = `INSERT INTO tags (lineage_id, tag, created_at) VALUES ('` + lineageID + `', 'urgent', 1);`
	case "comment":
		sql = `INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('` + lineageID + `', 'keep me', 1, 1);`
	default:
		t.Fatalf("unknown annotation %q", kind)
	}
	if err := db.Exec(sql); err != nil {
		t.Fatal(err)
	}
}

// writeTranscript drops a small transcript file in dir and returns its path.
func writeTranscript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func snapshotRow(t *testing.T, db *sqlitex.Runner, sessionID string) (path string, size, mtime, copiedAt int64, sourcePath *string) {
	t.Helper()
	var rows []struct {
		Path        string  `json:"path"`
		SourceSize  int64   `json:"source_size"`
		SourceMtime int64   `json:"source_mtime"`
		CopiedAt    *int64  `json:"copied_at"`
		SourcePath  *string `json:"source_path"`
	}
	q := `SELECT path, source_size, source_mtime, copied_at, source_path FROM snapshots WHERE session_id = '` + sessionID + `';`
	if err := db.Query(q, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one snapshots row for %q, got %d", sessionID, len(rows))
	}
	copied := int64(0)
	if rows[0].CopiedAt != nil {
		copied = *rows[0].CopiedAt
	}
	return rows[0].Path, rows[0].SourceSize, rows[0].SourceMtime, copied, rows[0].SourcePath
}

// TestSnapshotRecordsTheSourcePath covers the snapshot-source-path task: a
// copy records the agent's own transcript path in snapshots.source_path, so
// that a later rebuild which drops the session row still leaves the copy
// saying where it came from - the path T27 re-indexes and T19 restores.
func TestSnapshotRecordsTheSourcePath(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()
	now := time.Unix(1_700_000_000, 0)

	transcript := writeTranscript(t, src, "s.jsonl", "the body")
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin", transcriptPath: transcript})
	annotate(t, db, "lin", "star")

	if _, err := Snapshot(db, config.Retain{Annotated: true}, dir, now); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, sourcePath := snapshotRow(t, db, "claude:p:1")
	if sourcePath == nil || *sourcePath != transcript {
		t.Errorf("snapshots.source_path = %v, want %q", sourcePath, transcript)
	}
}

// TestAnnotatedSessionsAreCopied covers the default keep rule: a session is
// copied when its lineage carries any one of the four annotations a user
// can put on it, and a plain session is left alone - the whole point of
// "keep what the user already said matters".
func TestAnnotatedSessionsAreCopied(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()
	now := time.Unix(1_700_000_000, 0)

	kinds := []string{"star", "name", "tag", "comment", "plain"}
	for i, kind := range kinds {
		id := string(rune('a' + i))
		transcript := writeTranscript(t, src, id+".jsonl", "body of "+id)
		seedSession(t, db, session{id: "claude:p:" + id, source: "claude", install: "cc", sourceID: id, lineage: "lin_" + id, transcriptPath: transcript})
		if kind != "plain" {
			annotate(t, db, "lin_"+id, kind)
		}
	}

	res, err := Snapshot(db, config.Retain{Annotated: true}, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 4 || res.Copied != 4 {
		t.Fatalf("Kept/Copied = %d/%d, want 4/4", res.Kept, res.Copied)
	}
	if res.Missing != 0 || res.Failed != 0 || res.Err != nil {
		t.Fatalf("Missing/Failed/Err = %d/%d/%v, want 0/0/<nil>", res.Missing, res.Failed, res.Err)
	}

	for i, kind := range kinds {
		id := string(rune('a' + i))
		if kind == "plain" {
			if _, err := os.Stat(SnapshotPath(dir, "claude", "cc", id, id+".jsonl")); !os.IsNotExist(err) {
				t.Errorf("unannotated session %s: expected no copy, got err=%v", id, err)
			}
			continue
		}
		dst := SnapshotPath(dir, "claude", "cc", id, filepath.Join(src, id+".jsonl"))
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("reading the copy of %s: %v", id, err)
		}
		if string(got) != "body of "+id {
			t.Errorf("copy of %s = %q, want %q", id, got, "body of "+id)
		}
		st, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("copy of %s has mode %o, want 600", id, perm)
		}
		p, size, _, copied, _ := snapshotRow(t, db, "claude:p:"+id)
		if p != dst || size != int64(len("body of "+id)) || copied != now.Unix() {
			t.Errorf("snapshots row for %s = (%q, %d, %d), want (%q, %d, %d)", id, p, size, copied, dst, len("body of "+id), now.Unix())
		}
	}
}

// TestSourceRuleKeepsOnlyThatSource covers retain.sources: with annotated
// off, naming one source keeps exactly that source's sessions and nothing
// else.
func TestSourceRuleKeepsOnlyThatSource(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()

	omp := writeTranscript(t, src, "omp.jsonl", "omp body")
	claude := writeTranscript(t, src, "claude.jsonl", "claude body")
	seedSession(t, db, session{id: "omp:p:1", source: "omp", install: "i", sourceID: "1", lineage: "lin_omp", transcriptPath: omp})
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin_claude", transcriptPath: claude})

	res, err := Snapshot(db, config.Retain{Sources: []string{"omp"}}, dir, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Copied != 1 {
		t.Fatalf("Kept/Copied = %d/%d, want 1/1", res.Kept, res.Copied)
	}
	if _, err := os.Stat(SnapshotPath(dir, "omp", "i", "1", omp)); err != nil {
		t.Errorf("expected the omp copy: %v", err)
	}
	if _, err := os.Stat(SnapshotPath(dir, "claude", "cc", "1", claude)); !os.IsNotExist(err) {
		t.Errorf("expected no claude copy, got err=%v", err)
	}
}

// TestPathRuleKeepsOnlyThatFolder covers retain.paths: a cwd GLOB keeps the
// sessions under it, and the patterns are expanded by config.Load exactly
// like hide.paths, so '*' is enough to cross '/'.
func TestPathRuleKeepsOnlyThatFolder(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()
	keep := filepath.Join(t.TempDir(), "keep")

	inside := writeTranscript(t, src, "inside.jsonl", "inside")
	outside := writeTranscript(t, src, "outside.jsonl", "outside")
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin_in", cwd: filepath.Join(keep, "sub"), transcriptPath: inside})
	seedSession(t, db, session{id: "claude:p:2", source: "claude", install: "cc", sourceID: "2", lineage: "lin_out", cwd: filepath.Join(t.TempDir(), "other"), transcriptPath: outside})

	rules := config.Retain{Paths: []string{filepath.Join(keep, "*")}}
	res, err := Snapshot(db, rules, dir, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Copied != 1 {
		t.Fatalf("Kept/Copied = %d/%d, want 1/1", res.Kept, res.Copied)
	}
	if _, err := os.Stat(SnapshotPath(dir, "claude", "cc", "1", inside)); err != nil {
		t.Errorf("expected the session under the kept folder: %v", err)
	}
	if _, err := os.Stat(SnapshotPath(dir, "claude", "cc", "2", outside)); !os.IsNotExist(err) {
		t.Errorf("expected no copy for the session outside the kept folder, got err=%v", err)
	}
}

// TestExcludeWinsOverAnnotated covers the ordering rule retain.ExcludePaths
// documents: exclusion is decided last, so an annotated session under an
// excluded folder is still not copied.
func TestExcludeWinsOverAnnotated(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()
	scratch := t.TempDir()
	wanted := filepath.Join(t.TempDir(), "wanted")

	skipped := writeTranscript(t, src, "skipped.jsonl", "scratch")
	kept := writeTranscript(t, src, "kept.jsonl", "wanted")
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin_skip", cwd: scratch, transcriptPath: skipped})
	seedSession(t, db, session{id: "claude:p:2", source: "claude", install: "cc", sourceID: "2", lineage: "lin_keep", cwd: wanted, transcriptPath: kept})
	annotate(t, db, "lin_skip", "star")
	annotate(t, db, "lin_keep", "star")

	rules := config.Retain{Annotated: true, ExcludePaths: []string{scratch}}
	res, err := Snapshot(db, rules, dir, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Copied != 1 {
		t.Fatalf("Kept/Copied = %d/%d, want 1/1", res.Kept, res.Copied)
	}
	if _, err := os.Stat(SnapshotPath(dir, "claude", "cc", "1", skipped)); !os.IsNotExist(err) {
		t.Errorf("expected the excluded session not to be copied, got err=%v", err)
	}
	if _, err := os.Stat(SnapshotPath(dir, "claude", "cc", "2", kept)); err != nil {
		t.Errorf("expected the annotated session outside the exclude to be copied: %v", err)
	}
}

// TestSessionWithoutTranscriptIsSkipped covers a database-backed source
// whose history LazyRecall never copies: a hermes row has a NULL
// transcript_path and is not one of the transcript sources, so it is
// passed over whatever the rules say - and even when it does carry a
// transcript path (which a real hermes session never does), the source
// filter alone still excludes it. config.Load refuses to accept "hermes"
// in retain.sources for the same reason, but Snapshot must not rely on that
// front door to keep a session of a database-backed source out.
func TestSessionWithoutTranscriptIsSkipped(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()

	seedSession(t, db, session{id: "hermes:p:1", source: "hermes", install: "h", sourceID: "1", lineage: "lin_hermes"})
	annotate(t, db, "lin_hermes", "star")

	// A path that would be perfectly copiable if the source rule let it
	// through.
	contrived := writeTranscript(t, src, "hermes.jsonl", "not ours to copy")
	seedSession(t, db, session{id: "hermes:p:2", source: "hermes", install: "h", sourceID: "2", lineage: "lin_hermes2", transcriptPath: contrived})
	annotate(t, db, "lin_hermes2", "star")

	res, err := Snapshot(db, config.Retain{Annotated: true, Sources: []string{"omp"}}, dir, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 0 {
		t.Errorf("Kept = %d, want 0: a session of a database-backed source is never selected", res.Kept)
	}
	if _, err := os.Stat(SnapshotPath(dir, "hermes", "h", "2", contrived)); !os.IsNotExist(err) {
		t.Errorf("expected no copy of a hermes session, got err=%v", err)
	}
}

// TestCopyFailureIsRecordedAndThePassCarriesOn covers the failure path: one
// session whose copy cannot be completed is counted as Failed with its
// error kept, and the sessions after it are still copied. The failure is
// forced by putting a directory where the copy belongs, so the rename onto
// it cannot succeed - the one step of the copy that takes place after the
// temporary file exists, which is what makes it also prove the leftover
// .tmp is cleaned up.
func TestCopyFailureIsRecordedAndThePassCarriesOn(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()

	bad := writeTranscript(t, src, "bad.jsonl", "cannot land")
	good := writeTranscript(t, src, "good.jsonl", "lands fine")
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin_bad", transcriptPath: bad})
	seedSession(t, db, session{id: "claude:p:2", source: "claude", install: "cc", sourceID: "2", lineage: "lin_good", transcriptPath: good})
	annotate(t, db, "lin_bad", "star")
	annotate(t, db, "lin_good", "star")

	blocked := SnapshotPath(dir, "claude", "cc", "1", bad)
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := Snapshot(db, config.Retain{Annotated: true}, dir, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("a per-file failure must be reported in the Result, not returned: %v", err)
	}
	if res.Kept != 2 || res.Failed != 1 || res.Copied != 1 {
		t.Fatalf("Kept/Failed/Copied = %d/%d/%d, want 2/1/1", res.Kept, res.Failed, res.Copied)
	}
	if res.Err == nil {
		t.Error("Err = nil, want the first per-file error")
	}
	got, err := os.ReadFile(SnapshotPath(dir, "claude", "cc", "2", good))
	if err != nil || string(got) != "lands fine" {
		t.Errorf("the session after the failing one: content %q err %v, want %q and no error", got, err, "lands fine")
	}
	var leftovers []string
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".tmp") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) > 0 {
		t.Errorf("left temporary files behind: %v", leftovers)
	}
}

// TestSecondRunIsUnchangedAndAppendCopiesAgain covers the two states a
// second pass can find: nothing changed, so the copy is not rewritten
// (proved by its mtime, which a rewrite would move), and the agent appended
// to the file, so the copy is refreshed and the new bytes are there.
func TestSecondRunIsUnchangedAndAppendCopiesAgain(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()
	now := time.Unix(1_700_000_000, 0)

	transcript := writeTranscript(t, src, "s.jsonl", "first")
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin", transcriptPath: transcript})
	annotate(t, db, "lin", "star")

	if _, err := Snapshot(db, config.Retain{Annotated: true}, dir, now); err != nil {
		t.Fatal(err)
	}
	dst := SnapshotPath(dir, "claude", "cc", "1", transcript)
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Snapshot(db, config.Retain{Annotated: true}, dir, now.Add(1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Unchanged != 1 || res.Copied != 0 {
		t.Fatalf("second pass: Kept/Unchanged/Copied = %d/%d/%d, want 1/1/0", res.Kept, res.Unchanged, res.Copied)
	}
	after, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("unchanged pass rewrote the copy: mtime %v -> %v", before.ModTime(), after.ModTime())
	}

	if err := os.WriteFile(transcript, []byte("first and then more"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = Snapshot(db, config.Retain{Annotated: true}, dir, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Copied != 1 || res.Unchanged != 0 {
		t.Fatalf("after the append: Copied/Unchanged = %d/%d, want 1/0", res.Copied, res.Unchanged)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first and then more" {
		t.Errorf("copy after the append = %q, want %q", got, "first and then more")
	}
}

// TestDeletedTranscriptIsMissingAndTheCopyStays covers the case the whole
// feature exists for: the agent deleted its transcript, so there is nothing
// to copy - and the copy made earlier is still there, untouched.
func TestDeletedTranscriptIsMissingAndTheCopyStays(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()
	now := time.Unix(1_700_000_000, 0)

	transcript := writeTranscript(t, src, "s.jsonl", "the body")
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin", transcriptPath: transcript})
	annotate(t, db, "lin", "comment")

	if _, err := Snapshot(db, config.Retain{Annotated: true}, dir, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}

	res, err := Snapshot(db, config.Retain{Annotated: true}, dir, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Missing != 1 || res.Copied != 0 {
		t.Fatalf("Kept/Missing/Copied = %d/%d/%d, want 1/1/0", res.Kept, res.Missing, res.Copied)
	}
	got, err := os.ReadFile(SnapshotPath(dir, "claude", "cc", "1", transcript))
	if err != nil {
		t.Fatalf("the earlier copy should still be there: %v", err)
	}
	if string(got) != "the body" {
		t.Errorf("the earlier copy = %q, want %q", got, "the body")
	}
}

// TestSnapshotPathStaysInsideTheFolder covers the one thing a copy path
// must never do: leave the folder. A session id comes from another program,
// so a traversal in it must end up as inert characters inside dir, and the
// transcript's extension must survive so the copy is still a .jsonl.
func TestSnapshotPathStaysInsideTheFolder(t *testing.T) {
	dir := t.TempDir()
	got := SnapshotPath(dir, "claude", "cc", "../../etc/x", "/tmp/whatever.jsonl")
	if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
		t.Errorf("SnapshotPath = %q, want it inside %q", got, dir)
	}
	// The id's separators must be inert characters, not segments: the whole
	// path below dir is one directory per source/install plus one file.
	rel, err := filepath.Rel(dir, got)
	if err != nil {
		t.Fatal(err)
	}
	if parts := strings.Split(rel, string(filepath.Separator)); len(parts) != 3 || parts[0] != "claude" || parts[1] != "cc" {
		t.Errorf("SnapshotPath = %q, want exactly <dir>/claude/cc/<one file>, got parts %q", got, parts)
	}
	if filepath.Ext(got) != ".jsonl" {
		t.Errorf("SnapshotPath = %q, want the .jsonl extension kept", got)
	}
	if got := SnapshotPath(dir, "claude", "cc", "..", "/tmp/a.jsonl"); filepath.Base(got) != "__.jsonl" {
		t.Errorf("SnapshotPath(dots) = %q, want a base of %q", got, "__.jsonl")
	}
}

// TestNoKeepRuleDoesNothing covers the empty-rules case: with nothing asked
// for, the pass reports zero and touches nothing - not even the query.
func TestNoKeepRuleDoesNothing(t *testing.T) {
	db := testDB(t)
	src, dir := t.TempDir(), t.TempDir()

	for i, kind := range []string{"star", "tag"} {
		id := string(rune('a' + i))
		transcript := writeTranscript(t, src, id+".jsonl", "body")
		seedSession(t, db, session{id: "claude:p:" + id, source: "claude", install: "cc", sourceID: id, lineage: "lin_" + id, transcriptPath: transcript})
		annotate(t, db, "lin_"+id, kind)
	}

	res, err := Snapshot(db, config.Retain{}, dir, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{}) {
		t.Errorf("Result = %+v, want the zero value", res)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("expected the folder to stay empty, got %d entries (err=%v)", len(entries), err)
	}
}

// TestDirForUsesTheDefaultUnderTheDataDir covers DirFor's default: with no
// retain.dir configured the folder is LazyRecall's own snapshots folder
// under the data directory, and LAZYRECALL_HOME is what moves that data
// directory - so a test can point the whole thing at a temp dir.
func TestDirForUsesTheDefaultUnderTheDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LAZYRECALL_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if got, want := DirFor(config.Retain{}), filepath.Join(home, "snapshots"); got != want {
		t.Errorf("DirFor(default) = %q, want %q", got, want)
	}
	if got, want := DirFor(config.Retain{Dir: "/tmp/elsewhere"}), "/tmp/elsewhere"; got != want {
		t.Errorf("DirFor(configured) = %q, want %q", got, want)
	}
}

// TestAfterRefreshReadsTheConfig covers the entry point every refresh
// calls: it loads the config itself, so a configured retain.dir and the
// default rule apply without the caller passing either.
func TestAfterRefreshReadsTheConfig(t *testing.T) {
	home, cfgHome, src := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("LAZYRECALL_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

	db := testDB(t)
	transcript := writeTranscript(t, src, "s.jsonl", "body")
	seedSession(t, db, session{id: "claude:p:1", source: "claude", install: "cc", sourceID: "1", lineage: "lin", transcriptPath: transcript})
	annotate(t, db, "lin", "star")

	res, err := AfterRefresh(db, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Copied != 1 {
		t.Fatalf("Kept/Copied = %d/%d, want 1/1", res.Kept, res.Copied)
	}
	if _, err := os.Stat(filepath.Join(home, "snapshots", "claude", "cc", "1.jsonl")); err != nil {
		t.Errorf("expected the copy under the data directory's default snapshots folder: %v", err)
	}
}
