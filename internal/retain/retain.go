// Package retain copies the transcripts the user wants kept - the keep
// rules in config.Retain: annotated sessions, whole sources, whole cwd
// patterns - into LazyRecall's own folder, while the agent's own file still
// exists. Agents delete old transcripts (Claude Code's cleanupPeriodDays,
// for one), and a session row on the disposable index cannot be re-derived
// once the transcript is gone, so the copy is the last record of a session
// worth coming back to; T18 reads the copies, T19 restores one on resume.
//
// This package writes in exactly two places, and nowhere else: files inside
// the snapshot folder (whose default is profile.DataDir()/snapshots, i.e.
// under LazyRecall's own data directory), and rows in the durable snapshots
// table that record them. It never opens a source's file for writing, never
// deletes anything outside the snapshot folder, and never asks another
// tool's database for anything.
package retain

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wimcraft/lazyrecall/internal/config"
	"github.com/wimcraft/lazyrecall/internal/profile"
	"github.com/wimcraft/lazyrecall/internal/sqlitex"
)

// transcriptSources are the sources with a transcript file to copy. The
// database-backed ones (hermes, goose, opencode, kilo, antigravity) keep
// their history in their own SQLite database, which LazyRecall never copies
// - so a session of one of those is never selected, whatever the rules say.
var transcriptSources = []string{"claude", "pi", "omp"}

// DirFor is the folder snapshots go in: the configured retain.dir, or the
// default under LazyRecall's own data directory when none is configured.
// Resolved here, not in internal/config, for the reason `config show`'s
// counterpart spells out: internal/config cannot import internal/profile
// (profile imports config), so config.Load leaves Dir empty and every
// reader of that empty value has to know the default itself.
func DirFor(rules config.Retain) string {
	if rules.Dir != "" {
		return rules.Dir
	}
	return filepath.Join(profile.DataDir(), "snapshots")
}

// SnapshotPath is where one session's transcript is copied to:
// dir/<source>/<install>/<safe id><ext>. Nesting by source and install is
// what keeps two agents' identically-numbered session ids (and two accounts'
// same install name) from colliding in one flat folder, and it makes the
// folder readable by hand - `ls snapshots/claude/*/`. The extension is kept
// so a copy is still recognisably a transcript when read outside LazyRecall.
func SnapshotPath(dir, source, install, sourceSessionID, transcriptPath string) string {
	return filepath.Join(dir, source, install, safe(sourceSessionID)+filepath.Ext(transcriptPath))
}

// safe turns a source's own session id into one path segment. Session ids
// come from another program, so one holding a "/" or a ".." would otherwise
// decide where the copy lands and could climb out of the snapshot folder
// entirely, or overwrite a file outside it; every character outside
// [A-Za-z0-9._-] becomes "_", and a name that is nothing but dots becomes
// underscores too, since "." and ".." are path-navigation segments however
// they reached here. An empty id becomes "_" for the same reason the dots
// rule exists: the result is always one ordinary, non-empty file name.
func safe(sourceSessionID string) string {
	var b strings.Builder
	for _, r := range sourceSessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if strings.Trim(out, ".") == "" {
		out = strings.ReplaceAll(out, ".", "_")
	}
	if out == "" {
		out = "_"
	}
	return out
}

// Result is what one Snapshot pass did. Kept is every session the rules
// select - the other four counts partition it, so Copied+Unchanged+Missing+
// Failed always equals Kept.
type Result struct {
	// Kept is the number of sessions the keep rules selected.
	Kept int
	// Copied is the number of transcripts copied (fresh, or again because
	// the agent's file changed since the last copy).
	Copied int
	// Unchanged is the number of sessions whose earlier copy still matches
	// the agent's current file, so nothing was written.
	Unchanged int
	// Missing is the number of sessions whose transcript the agent has
	// already deleted: there is nothing left to copy, and any earlier copy
	// stays as it is.
	Missing int
	// Failed is the number of sessions whose copy failed - a permission
	// problem, a full disk, an unreadable folder. One bad file must not
	// abandon the others.
	Failed int
	// Err is the first per-file failure, kept for the one line of output
	// that reports it. The error Snapshot itself returns is for a database
	// failure, which is a different thing entirely.
	Err error
}

// Snapshot copies the kept sessions' transcripts into dir and records each
// copy in the snapshots table. It is idempotent: a second pass over files
// that have not changed writes nothing, and a pass over a session whose
// transcript is already gone does nothing but count it. Sessions that are
// not kept are never opened, and nothing is ever copied out of the folder.
func Snapshot(db *sqlitex.Runner, rules config.Retain, dir string, now time.Time) (Result, error) {
	var res Result

	// No keep rule at all selects nothing - and the query below would have
	// no keep clause to put in its OR, which is not a shape worth spelling
	// out in SQL. Every exclude path is a rule about what NOT to keep, so
	// excludes alone keep nothing either.
	if !rules.Annotated && len(rules.Sources) == 0 && len(rules.Paths) == 0 {
		return res, nil
	}

	sqlText, pf, err := selectSQL(db, rules)
	if err != nil {
		return res, err
	}
	defer pf.Close()

	var rows []struct {
		ID              string  `json:"id"`
		Source          string  `json:"source"`
		Install         *string `json:"install"`
		SourceSessionID string  `json:"source_session_id"`
		TranscriptPath  string  `json:"transcript_path"`
		SnapshotPath    *string `json:"snapshot_path"`
		SnapshotSize    *int64  `json:"snapshot_size"`
		SnapshotMtime   *int64  `json:"snapshot_mtime"`
	}
	if err := db.Query(sqlText, &rows); err != nil {
		return res, fmt.Errorf("retain: selecting the sessions to snapshot: %w", err)
	}

	for _, row := range rows {
		res.Kept++
		install := ""
		if row.Install != nil {
			install = *row.Install
		}

		st, err := os.Stat(row.TranscriptPath)
		if err != nil {
			if os.IsNotExist(err) {
				// The agent got there first. Nothing to copy, and an
				// earlier copy - the whole reason this table exists - is
				// left exactly where it is.
				res.Missing++
				continue
			}
			res.fail(err)
			continue
		}

		size, mtime := st.Size(), st.ModTime().UnixNano()
		if row.SnapshotPath != nil && row.SnapshotSize != nil && row.SnapshotMtime != nil &&
			*row.SnapshotSize == size && *row.SnapshotMtime == mtime {
			if _, err := os.Stat(*row.SnapshotPath); err == nil {
				res.Unchanged++
				continue
			}
		}

		dst := SnapshotPath(dir, row.Source, install, row.SourceSessionID, row.TranscriptPath)
		if err := copyFile(row.TranscriptPath, dst); err != nil {
			res.fail(err)
			continue
		}
		if err := upsertSnapshot(db, row.ID, dst, size, mtime, now.Unix()); err != nil {
			return res, err
		}
		res.Copied++
	}
	return res, nil
}

// AfterRefresh is what every refresh calls after its pass: load the config,
// and snapshot whatever the retain rules keep. Snapshot failures inside it
// (a file that could not be copied) are reported in the Result rather than
// returned, so a caller that ignores the Result - the browser and every
// command's automatic refresh - is not made to fail because one transcript
// could not be read.
func AfterRefresh(db *sqlitex.Runner, now time.Time) (Result, error) {
	cfg, err := config.Load()
	if err != nil {
		return Result{}, err
	}
	return Snapshot(db, cfg.Retain, DirFor(cfg.Retain), now)
}

func (r *Result) fail(err error) {
	r.Failed++
	if r.Err == nil {
		r.Err = err
	}
}

// selectSQL builds the one query that picks out the kept sessions, along
// with the ParamFile whose values must stay alive until it has run. Every
// rule value (a source name, a cwd pattern) comes from the config file, so
// every one of them travels as a bound parameter, exactly like the search
// package's own cwd patterns; the source names are the same program
// constants there and here.
//
// The snapshots columns ride along on a LEFT JOIN rather than a second
// query per session: whether a copy is still current is decided from the
// size and mtime recorded when it was made, so a pass over many unchanged
// sessions stays one query plus one Stat each.
func selectSQL(db *sqlitex.Runner, rules config.Retain) (string, *sqlitex.ParamFile, error) {
	type rule struct{ key, value string }
	var sources, paths []rule
	params := map[string]any{}
	for i, src := range rules.Sources {
		key := fmt.Sprintf("keep_source_%d", i)
		sources = append(sources, rule{key, src})
		params[key] = src
	}
	for i, p := range rules.Paths {
		key := fmt.Sprintf("keep_path_%d", i)
		paths = append(paths, rule{key, p})
		params[key] = p
	}
	for i, p := range rules.ExcludePaths {
		params[fmt.Sprintf("skip_path_%d", i)] = p
	}

	pf, err := db.WriteParams(params)
	if err != nil {
		return "", nil, err
	}

	var keep []string
	if rules.Annotated {
		keep = append(keep, `(l.custom_name IS NOT NULL OR l.starred_at IS NOT NULL
			OR EXISTS (SELECT 1 FROM tags WHERE tags.lineage_id = s.lineage_id)
			OR EXISTS (SELECT 1 FROM comments WHERE comments.lineage_id = s.lineage_id))`)
	}
	for _, r := range sources {
		keep = append(keep, "s.source = "+pf.Ref(r.key))
	}
	for _, r := range paths {
		keep = append(keep, "s.cwd GLOB "+pf.Ref(r.key))
	}

	var skip []string
	for i := range rules.ExcludePaths {
		// A NULL cwd matches no pattern, so it is never excluded - the same
		// guard the search package's hide.paths patterns carry.
		skip = append(skip, "(s.cwd IS NULL OR s.cwd NOT GLOB "+pf.Ref(fmt.Sprintf("skip_path_%d", i))+")")
	}

	q := `SELECT s.id, s.source, s.install, s.source_session_id, s.transcript_path,
			sn.path AS snapshot_path, sn.source_size AS snapshot_size, sn.source_mtime AS snapshot_mtime
		FROM sessions s
		LEFT JOIN lineages l ON l.id = s.lineage_id
		LEFT JOIN snapshots sn ON sn.session_id = s.id
		WHERE s.source IN ('` + strings.Join(transcriptSources, "', '") + `')
			AND s.transcript_path IS NOT NULL AND s.transcript_path != ''
			AND (` + strings.Join(keep, " OR ") + `)`
	if len(skip) > 0 {
		q += " AND " + strings.Join(skip, " AND ")
	}
	q += ";"
	return q, pf, nil
}

// copyFile writes src's bytes to dst without ever leaving a half-written
// file where a reader could find it: the bytes go to dst+".tmp" first and
// only a completed, flushed, closed file is renamed onto dst, which on
// every filesystem LazyRecall runs on is an atomic replace. A failure at
// any point removes the temporary file and leaves whatever was at dst
// untouched - an older copy is worth more than a truncated new one.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// upsertSnapshot records one copy in the durable snapshots table. The size
// and mtime are the source file's own, not the copy's: they are what the
// next pass compares the agent's file against to decide that the copy is
// still current, and the copy's own mtime would drift with the copy.
func upsertSnapshot(db *sqlitex.Runner, sessionID, path string, size, mtime, copiedAt int64) error {
	b := db.NewBatch()
	err := b.BulkUpsert("snapshots",
		[]string{"session_id"},
		[]string{"session_id", "path", "source_size", "source_mtime", "copied_at"},
		[]map[string]any{{
			"session_id":   sessionID,
			"path":         path,
			"source_size":  size,
			"source_mtime": mtime,
			"copied_at":    copiedAt,
		}})
	if err != nil {
		return err
	}
	if err := b.Run(); err != nil {
		return fmt.Errorf("retain: recording the snapshot of %s: %w", sessionID, err)
	}
	return nil
}
