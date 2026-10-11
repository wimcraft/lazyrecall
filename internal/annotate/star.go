package annotate

import (
	"fmt"
	"time"

	"github.com/wimcraft/lazyrecall/internal/sqlitex"
)

// SetStarred stars or unstars a lineage, the marker the user puts on a
// session from inside lazyrecall itself so it can be found again later
// (like starring an email). The star lives on the durable lineages table,
// exactly like archived_at and custom_name, so the user's decision
// survives the next refresh --full, which drops and rebuilds sessions, and
// moves with a continuation the way a lineage's other annotations do.
//
// Starring stores the current time; the row marker and filter that read it
// back are separate tasks, but keeping the moment a lineage was starred -
// as archived_at does for archiving - costs nothing now and is not
// recoverable later. Unstarring stores NULL. Both are idempotent, since
// the timestamp is set rather than conditionally inserted, so starring
// again simply refreshes when it happened.
func SetStarred(db *sqlitex.Runner, lineageID string, starred bool, now time.Time) error {
	if !starred {
		return clearStarred(db, lineageID)
	}
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID, "now": now.Unix()})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET starred_at = " + pf.Ref("now") + " WHERE id = " + pf.Ref("lineage_id") + ";")
}

func clearStarred(db *sqlitex.Runner, lineageID string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET starred_at = NULL WHERE id = " + pf.Ref("lineage_id") + ";")
}

// IsStarred reports whether a lineage is currently starred, mirroring
// IsArchived: a star is a timestamp whose presence is the boolean, so
// "starred" means the column is set to anything, not to some particular
// value.
func IsStarred(db *sqlitex.Runner, lineageID string) (bool, error) {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return false, err
	}
	defer pf.Close()

	var rows []struct {
		StarredAt *int64 `json:"starred_at"`
	}
	q := "SELECT starred_at FROM lineages WHERE id = " + pf.Ref("lineage_id") + ";"
	if err := db.Query(q, &rows); err != nil {
		return false, fmt.Errorf("annotate: checking starred status of %s: %w", lineageID, err)
	}
	if len(rows) == 0 {
		return false, fmt.Errorf("annotate: no such lineage %q", lineageID)
	}
	return rows[0].StarredAt != nil, nil
}
