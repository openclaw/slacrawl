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
