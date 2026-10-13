package refresh

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/wimcraft/lazyrecall/internal/adapter"
	"github.com/wimcraft/lazyrecall/internal/session"
)

// snapshotRow is one row from LazyRecall's durable snapshots table: the
// session it was taken for, the copy's own path, and the agent's original
// path the copy was made from (source_path, added in schema v13). Rows whose
// source_path is NULL - sessions already gone before that column existed -
// are not loaded at all, since there is no path to re-index them under.
type snapshotRow struct {
	SessionID  string `json:"session_id"`
	Path       string `json:"path"`
	SourcePath string `json:"source_path"`
}

// loadSnapshots reads every snapshot that still remembers the agent's own
// transcript path. Called once per refresh pass, after a full rebuild has
// cleared the disposable index but before the adapter loop, so each
// (source, install) pair can offer its own missing sessions back through
// the normal pipeline (T27).
func (r *Refresher) loadSnapshots() ([]snapshotRow, error) {
	var rows []snapshotRow
	if err := r.DB.Query(`SELECT session_id, path, source_path FROM snapshots WHERE source_path IS NOT NULL AND source_path != '';`, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// snapshotDiscovered returns the extra adapter.Discovered entries for one
// (source, install) pair: sessions the adapter could not discover this pass
// because the agent's transcript file is gone, but for which LazyRecall
// still holds a copy. Each entry reads its bytes from the copy (ReadFrom)
// while TranscriptPath carries the agent's original path, so the re-indexed
// session is stored exactly as any other snapshotted session (internal/search
// redirects such a row at its copy; T19's resume restores it there).
//
// A row only qualifies when its copy still exists (its size is the
// reported FileSize) and its original does not: an original that is present
// belongs to the adapter's own Discover, and shadowing it here would double
// the session. The session id's own prefix - "<source>:<install>:" - already
// names the pair, so the rest of it is the source session id, the same split
// the rest of the pipeline relies on (session.Session.ID).
func snapshotDiscovered(source, install string, snaps []snapshotRow, discovered []adapter.Discovered) []adapter.Discovered {
	prefix := source + ":" + install + ":"
	known := make(map[string]bool, len(discovered))
	for _, d := range discovered {
		known[d.TranscriptPath] = true
	}

	var out []adapter.Discovered
	for _, snap := range snaps {
		if !strings.HasPrefix(snap.SessionID, prefix) {
			continue
		}
		if known[snap.SourcePath] {
			// The adapter found this transcript itself. It owns the session;
			// the copy is not needed and would collide with it.
			continue
		}
		if _, err := os.Lstat(snap.SourcePath); !errors.Is(err, fs.ErrNotExist) {
			// The original is still there (any other stat error is not a
			// definite "gone" either), so the adapter is the one to find it -
			// never shadow it with a snapshot entry.
			continue
		}
		fi, err := os.Stat(snap.Path)
		if err != nil {
			// The copy itself is gone (the folder cleaned out by hand, say):
			// there is nothing left to read, so the session stays gone.
			continue
		}
		id := strings.TrimPrefix(snap.SessionID, prefix)
		out = append(out, adapter.Discovered{
			Session: session.Session{
				ID:              snap.SessionID,
				Source:          source,
				Profile:         install,
				SourceSessionID: id,
				EndState:        session.EndStateUnknown,
				Resumable:       true,
			},
			TranscriptPath: snap.SourcePath,
			ReadFrom:       snap.Path,
			FileSize:       fi.Size(),
		})
	}
	return out
}
