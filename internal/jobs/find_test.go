package jobs

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/database"
)

func TestFindTopLevelAndChildren(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	enqueue := func(typ, payload string, parent int64) int64 {
		t.Helper()
		j, err := repo.EnqueueChild(ctx, typ, payload, time.Now(), parent)
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	ids := func(list []Job) []int64 {
		out := []int64{}
		for _, j := range list {
			out = append(out, j.ID)
		}
		return out
	}

	old := enqueue(TypeScanDownloads, `{"title_id":7}`, 0)
	exec(`UPDATE jobs SET status = 'done' WHERE id = ?`, old)
	for i := range 100 {
		filler := enqueue(TypeVerifySource, `{"source_id":"s`+string(rune('a'+i%26))+string(rune('a'+i/26))+`"}`, 0)
		exec(`UPDATE jobs SET status = 'done' WHERE id = ?`, filler)
	}
	parent := enqueue(TypeRefreshTitle, `{}`, 0)
	c1 := enqueue(TypeRefreshTitle, `{"title_id":1}`, parent)
	c2 := enqueue(TypeRefreshTitle, `{"title_id":2}`, parent)
	c3 := enqueue(TypeRefreshTitle, `{"title_id":3}`, parent)
	exec(`UPDATE jobs SET status = 'done' WHERE id = ?`, c1)
	exec(`UPDATE jobs SET status = 'dead' WHERE id = ?`, c2)
	solo := enqueue(TypeScanDownloads, `{"title_id":1}`, 0)
	again := enqueue(TypeRefreshTitle, `{"title_id":3,"retry":1}`, 0)
	exec(`INSERT INTO jobs(type, status, payload_json, parent_id) VALUES (?, 'queued', '{"title_id":4}', 999)`, TypeScanDownloads)
	var orphan int64
	if err := db.QueryRowContext(ctx, `SELECT MAX(id) FROM jobs`).Scan(&orphan); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		f    Filter
		want []int64
	}{
		"by title":            {Filter{TitleIDs: []int64{1}}, []int64{solo, c1}},
		"newest per kind":     {Filter{TitleIDs: []int64{1, 2, 3}, Newest: true}, []int64{again, solo, c2, c1}},
		"outside window":      {Filter{TitleIDs: []int64{7}}, []int64{}},
		"no titles":           {Filter{TitleIDs: []int64{}}, []int64{}},
		"type and payload":    {Filter{Type: TypeScanDownloads, Payload: `{"title_id":1}`, Limit: 1}, []int64{solo}},
		"status newest first": {Filter{Statuses: []string{"queued"}, Limit: 2}, []int64{orphan, again}},
		"before cursor":       {Filter{Statuses: []string{"queued"}, BeforeID: solo}, []int64{c3, parent}},
	} {
		got, err := repo.Find(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids(got), tc.want) {
			t.Errorf("%s = %v, want %v", name, ids(got), tc.want)
		}
	}

	top, total, err := repo.TopLevel(ctx, "", "", 0, 3)
	if err != nil || total < 3 || !reflect.DeepEqual(ids(top), []int64{orphan, again, solo}) {
		t.Fatalf("TopLevel = %v total=%d, %v", ids(top), total, err)
	}
	kids, err := repo.Children(ctx, []int64{parent}, 2)
	if err != nil {
		t.Fatal(err)
	}
	k := kids[parent]
	if k == nil || k.Total != 3 || k.Counts["done"] != 1 || k.Counts["dead"] != 1 || k.Counts["queued"] != 1 || !reflect.DeepEqual(ids(k.Newest), []int64{c3, c2}) {
		t.Fatalf("Children = %+v", k)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	old := database.FormatTime(time.Now().Add(-48 * time.Hour))
	add := func(status, updated string, parent int64) int64 {
		t.Helper()
		res, err := db.ExecContext(ctx, `INSERT INTO jobs(type, status, payload_json, parent_id, updated_at) VALUES (?, ?, '{}', NULLIF(?, 0), ?)`, TypeScanDownloads, status, parent, updated)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	now := database.FormatTime(time.Now())
	oldDone := add("done", old, 0)
	oldCancelled := add("cancelled", old, 0)
	oldDead := add("dead", old, 0)
	keep := []int64{
		add("done", now, 0),
		add("failed", old, 0),
		add("queued", old, 0),
	}
	busyParent := add("done", old, 0)
	keep = append(keep, busyParent, add("queued", now, busyParent))
	doneParent := add("done", old, 0)
	doneChild := add("done", old, doneParent)

	n, err := repo.Prune(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 5 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	for _, id := range []int64{oldDone, oldCancelled, oldDead, doneParent, doneChild} {
		if _, err := repo.Get(ctx, id); err == nil {
			t.Errorf("job %d survived prune", id)
		}
	}
	for _, id := range keep {
		if _, err := repo.Get(ctx, id); err != nil {
			t.Errorf("job %d pruned: %v", id, err)
		}
	}
}
