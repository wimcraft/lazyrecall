package annotate

import (
	"testing"
	"time"

	"github.com/wimcraft/lazyrecall/internal/sqlitex"
)

// storedStarredAt reads the raw starred_at value for a lineage, so a test
// can assert the exact timestamp rather than only IsStarred's boolean.
func storedStarredAt(t *testing.T, db *sqlitex.Runner, lineageID string) *int64 {
	t.Helper()
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	var rows []struct {
		StarredAt *int64 `json:"starred_at"`
	}
	q := "SELECT starred_at FROM lineages WHERE id = " + pf.Ref("lineage_id") + ";"
	if err := db.Query(q, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one lineage row for %q, got %d", lineageID, len(rows))
	}
	return rows[0].StarredAt
}

func TestSetStarredRoundTrip(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	now := time.Unix(1_700_000_000, 0)
	if err := SetStarred(db, "lin1", true, now); err != nil {
		t.Fatal(err)
	}
	got, err := IsStarred(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("IsStarred = false, want true after starring")
	}
	if at := storedStarredAt(t, db, "lin1"); at == nil || *at != now.Unix() {
		t.Errorf("starred_at = %v, want %d", at, now.Unix())
	}
}

// TestSetStarredFalseClears covers unstaring: the marker is gone and the
// lineage reads back as not starred.
func TestSetStarredFalseClears(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	if err := SetStarred(db, "lin1", true, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}

	if err := SetStarred(db, "lin1", false, time.Unix(1_700_000_100, 0)); err != nil {
		t.Fatal(err)
	}
	got, err := IsStarred(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("IsStarred = true, want false after unstarring")
	}
	if at := storedStarredAt(t, db, "lin1"); at != nil {
		t.Errorf("starred_at = %v, want NULL after unstarring", at)
	}
}

// TestUnstarNeverStarredIsNotAnError covers clearing a star that was never
// set - the filter and row marker can both be in a state where the user
// unstars something already unstarred, which must be a no-op rather than a
// failure.
func TestUnstarNeverStarredIsNotAnError(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if err := SetStarred(db, "lin1", false, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("unstarring a never-starred lineage: %v", err)
	}
	got, err := IsStarred(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("IsStarred = true, want false for a lineage that was never starred")
	}
}
