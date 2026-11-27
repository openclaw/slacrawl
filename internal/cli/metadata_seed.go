package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"

	"github.com/openclaw/slacrawl/internal/slackapi"
	"github.com/openclaw/slacrawl/internal/store"
)

func (a *App) runMetadataSeed(ctx context.Context, args []string, configPath *string, format OutputFormat) error {
	fs := flag.NewFlagSet("acquire metadata-seed", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "approved channel inventory JSON")
	checksum := fs.String("manifest-sha256", "", "SHA-256 of independently approved inventory bytes")
	dest := fs.String("output-dir", "", "new absolute directory for private metadata seed and delivery scope")
	timeout := fs.Duration("timeout", 0, "positive total operation deadline")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *timeout <= 0 || *manifest == "" || !filepath.IsAbs(*dest) || filepath.Clean(*dest) != *dest {
		return errors.New("metadata-seed requires --manifest, --manifest-sha256, a new absolute --output-dir and positive --timeout")
	}
	if _, err := os.Lstat(*dest); !errors.Is(err, os.ErrNotExist) {
		return errors.New("metadata seed output must be a new directory")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	data, err := readPrivateInput(ctx, *manifest, deliveryScopeMaxBytes)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if *checksum != hex.EncodeToString(sum[:]) {
		return errors.New("approved inventory checksum mismatch")
	}
	var inventory struct {
		Schema, Policy, Workspace, ReaderApp, ReaderUser string
		Channels                                         []slackapi.ApprovedChannel
	}
	if json.Unmarshal(data, &inventory) != nil || inventory.Schema != "slack-approved-channel-inventory/v1" || inventory.Policy != "slack-report-channels/v1" {
		return errors.New("invalid approved channel inventory")
	}
	provider, err := a.acquisitionProvider(inventory.Workspace, configPath)
	if err != nil {
		return err
	}
	team, channels, err := provider.MetadataSeed(ctx, inventory.Workspace, inventory.ReaderApp, inventory.ReaderUser, inventory.Channels)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(*dest, 0o700); err != nil {
		return errors.New("cannot create new metadata seed directory")
	}
	archive, err := store.Open(filepath.Join(*dest, "source.sqlite"))
	if err != nil {
		return errors.New("cannot initialize new metadata archive")
	}
	defer archive.Close()
	if err := archive.UpsertWorkspace(ctx, team); err != nil {
		return errors.New("cannot store seed workspace")
	}
	doc := deliveryScopeDocument{Version: 1, WorkspaceID: team.ID, SyncStateKeys: []deliverySyncStateKey{}}
	for _, row := range channels {
		if err := archive.UpsertChannel(ctx, row.Channel); err != nil {
			return errors.New("cannot store admitted seed channel")
		}
		doc.Channels = append(doc.Channels, deliveryChannelAdmission{row.Channel.ID, row.Admission})
	}
	if err := archive.Close(); err != nil {
		return errors.New("cannot finish metadata archive")
	}
	scope, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dest, "scope.json"), scope, 0o600); err != nil {
		return errors.New("cannot finish metadata delivery scope")
	}
	return a.writeOutput("Metadata seed", map[string]any{"status": "prepared", "channels": len(channels), "manifest_sha256": *checksum}, format, false)
}
