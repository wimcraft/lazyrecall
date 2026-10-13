package annotate

import (
	"fmt"
	"strings"
	"time"

	"github.com/wimcraft/lazyrecall/internal/sqlitex"
)

// Archive marks a lineage archived at the current time (change
// add-archive-facility). The archive flag lives on the durable lineages
// table, never on the disposable sessions table, so the user's decision
// survives the next refresh --full, which drops and rebuilds sessions. It
// is idempotent: archiving something already archived keeps the original
// timestamp - "when did I archive this" must stay answerable, and a
// repeated command must not move it.
func Archive(db *sqlitex.Runner, lineageID string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID, "now": time.Now().Unix()})
	if err != nil {
		return err
	}
	defer pf.Close()
	// COALESCE(archived_at, now) preserves an existing timestamp instead of
	// overwriting it, which is what makes a double-archive a no-op.
	return db.Exec("UPDATE lineages SET archived_at = COALESCE(archived_at, " + pf.Ref("now") + ") WHERE id = " + pf.Ref("lineage_id") + ";")
}

// ArchiveMany archives every lineage in lineageIDs in a single statement,
// with the same COALESCE(archived_at, now) Archive uses, so a lineage that
// is already archived keeps its original timestamp. It exists for the
// browser's "archive everything the list shows" action: Archive runs one
// statement per id, and each Exec against the driver is a round trip, so a
// few hundred rows would take a few hundred of them where this takes one.
//
// Every id is a bound parameter of one ParamFile, never interpolated into
// the SQL string, and the IN list is built from the Refs alone - the ids
// themselves never appear in the statement text. An empty slice is
// answered without touching the database at all: there is nothing to
// archive, and an `IN ()` is not valid SQL.
func ArchiveMany(db *sqlitex.Runner, lineageIDs []string) error {
	if len(lineageIDs) == 0 {
		return nil
	}
	params := make(map[string]any, len(lineageIDs)+1)
	params["now"] = time.Now().Unix()
	fields := make([]string, 0, len(lineageIDs))
	for i, id := range lineageIDs {
		field := fmt.Sprintf("lineage_%d", i)
		params[field] = id
		fields = append(fields, field)
	}
	pf, err := db.WriteParams(params)
	if err != nil {
		return err
	}
	defer pf.Close()
	// The IN list is this ParamFile's own Refs, built after registration so
	// the tokens carry its id; the lineage ids themselves never appear in
	// the statement text.
	refs := make([]string, 0, len(fields))
	for _, field := range fields {
		refs = append(refs, pf.Ref(field))
	}
	return db.Exec("UPDATE lineages SET archived_at = COALESCE(archived_at, " + pf.Ref("now") + ") WHERE id IN (" + strings.Join(refs, ", ") + ");")
}

// Unarchive clears a lineage's archive flag, returning it to normal
// listings. Clearing an already-unarchived lineage is a no-op.
func Unarchive(db *sqlitex.Runner, lineageID string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET archived_at = NULL WHERE id = " + pf.Ref("lineage_id") + ";")
}

// IsArchived reports whether a lineage is currently archived.
func IsArchived(db *sqlitex.Runner, lineageID string) (bool, error) {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return false, err
	}
	defer pf.Close()

	var rows []struct {
		ArchivedAt *int64 `json:"archived_at"`
	}
	q := "SELECT archived_at FROM lineages WHERE id = " + pf.Ref("lineage_id") + ";"
	if err := db.Query(q, &rows); err != nil {
		return false, fmt.Errorf("annotate: checking archive status of %s: %w", lineageID, err)
	}
	if len(rows) == 0 {
		return false, fmt.Errorf("annotate: no such lineage %q", lineageID)
	}
	return rows[0].ArchivedAt != nil, nil
}

// AllArchived lists every archived lineage's id, most recently archived
// first, for `archive list`.
func AllArchived(db *sqlitex.Runner) ([]string, error) {
	var rows []struct {
		ID string `json:"id"`
	}
	if err := db.Query(`SELECT id FROM lineages WHERE archived_at IS NOT NULL ORDER BY archived_at DESC, id;`, &rows); err != nil {
		return nil, fmt.Errorf("annotate: listing archived lineages: %w", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out, nil
}
