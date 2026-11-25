package acquisition

import (
	"testing"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/slackapi"
)

func TestReconcilePreservesRetainedRevision(t *testing.T) {
	changes, err := slackapi.AcquisitionChanges([]byte(`{"ts":"1795046401.000001","user":"U1","text":"same"}`), "T1", "C1", slackapi.SourceBot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old := changes[0]
	old.Row = append(old.Row[:0:0], old.Row...)
	richer := "retained enrichment"
	old.Row[9].Value = &richer
	raw := `{"retained":true}`
	old.Row[16].Value = &raw
	if err = reconcile(changes, []remote.Change{old}); err != nil {
		t.Fatal(err)
	}
	if *changes[0].Row[16].Value != raw || *changes[0].Row[9].Value != richer {
		t.Fatal("lost retained bytes")
	}
	changed := "edited"
	changes[0].Row = append(changes[0].Row[:0:0], changes[0].Row...)
	changes[0].Row[8].Value = &changed
	if err = reconcile(changes, []remote.Change{old}); err == nil {
		t.Fatal("actual edit accepted")
	}
}

func TestRunWindowContinuesOrBackfills(t *testing.T) {
	at := func(day int) *time.Time { v := time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC); return &v }
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	if _, _, err := runWindow(Options{}, nil, now); err == nil {
		t.Fatal("first run without --since accepted")
	}
	since, until, err := runWindow(Options{}, at(20), now)
	if err != nil || !since.Equal(*at(20)) || !until.Equal(now) {
		t.Fatal("continuation window", since, until, err)
	}
	if _, _, err = runWindow(Options{Since: at(10)}, at(20), now); err == nil {
		t.Fatal("earlier --since without --until accepted")
	}
	if _, _, err = runWindow(Options{Since: at(1), Until: at(25)}, at(20), now); err == nil {
		t.Fatal("window overlapping the completed horizon accepted")
	}
	since, until, err = runWindow(Options{Since: at(1), Until: at(5)}, at(20), now)
	if err != nil || !since.Equal(*at(1)) || !until.Equal(*at(5)) {
		t.Fatal("backfill window", since, until, err)
	}
	if _, _, err = runWindow(Options{Since: at(5), Until: at(1)}, at(20), now); err == nil {
		t.Fatal("reversed window accepted")
	}
}
