package remotedelivery

import (
	"errors"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/store"
)

// ProjectPrivateSeed applies the existing PRIVATE profile to a complete local
// six-table seed. Empty tables still require their exact native column layout.
// This supplies rows only, not a manifest, transport or qualified baseline.
func ProjectPrivateSeed(seed store.NativeSeed) ([]remote.Change, error) {
	if len(seed.Tables) != len(privateTables) {
		return nil, errors.New("private seed requires all six content tables")
	}
	changes := []remote.Change{}
	for i, table := range seed.Tables {
		layout := privateTables[i]
		if table.Table != layout.table {
			return nil, errors.New("private seed table order differs")
		}
		positions, err := privateColumnPositions(layout, table.Columns)
		if err != nil {
			return nil, err
		}
		for _, values := range table.Rows {
			change, err := projectPrivateRow(layout, positions, values)
			if err != nil {
				return nil, err
			}
			changes = append(changes, change)
		}
	}
	return changes, nil
}
