package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/openclaw/slacrawl/internal/share"
)

// This envelope stays private: source bindings and replacement choices never
// belong in the public artifact or command output.
type exportPrivatePlan struct {
	Version          int                       `json:"version"`
	ProducerRevision string                    `json:"producer_revision"`
	Selection        share.ExportSelectionPlan `json:"selection"`
}

func (a *App) runExport(ctx context.Context, args []string, format OutputFormat) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		_, err := io.WriteString(a.Stdout, "Usage: slacrawl export <prepare|build|verify> [flags]\n\n  prepare --db PATH --selection PATH --out PRIVATE_PLAN\n  build   --db PATH --plan PRIVATE_PLAN --out NEW_DIR\n  verify  --db PATH --plan PRIVATE_PLAN --dir DIR\n\nOffline only. Selection and plan contain private data; keep them local.\n")
		return err
	}
	command := args[0]
	if command != "prepare" && command != "build" && command != "verify" {
		return errors.New("unknown export subcommand: use prepare, build, or verify")
	}
	fs := flag.NewFlagSet("export "+command, flag.ContinueOnError)
	db := fs.String("db", "", "existing archive file (required)")
	formatFlag := fs.String("format", string(format), "output format: text|json|log")
	jsonOut := fs.Bool("json", false, "json output")
	var input, output string
	if command == "prepare" {
		fs.StringVar(&input, "selection", "", "private selection JSON file (required)")
	} else {
		fs.StringVar(&input, "plan", "", "private prepared plan file (required)")
	}
	if command == "verify" {
		fs.StringVar(&output, "dir", "", "existing artifact directory (required)")
	} else {
		fs.StringVar(&output, "out", "", "new output path with an existing parent (required)")
	}
	if err := a.parseCommandFlags(fs, args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("export commands do not accept positional arguments")
	}
	if *db == "" || input == "" || output == "" {
		return errors.New("export requires explicit --db, input (--selection or --plan), and output (--out or --dir) paths")
	}
	format, err := resolveOutputFormat(*formatFlag, *jsonOut)
	if err != nil {
		return err
	}
	if command == "prepare" {
		revision, err := a.exportBuildRevision()
		if err != nil {
			return err
		}
		body, err := readExportInput(ctx, input)
		if err != nil {
			return err
		}
		var selection share.ExportSelection
		if err := json.Unmarshal(body, &selection); err != nil {
			return errors.New("invalid export selection JSON")
		}
		canonical, err := json.Marshal(selection)
		var compact bytes.Buffer
		if err != nil || json.Compact(&compact, body) != nil || !bytes.Equal(compact.Bytes(), canonical) {
			return errors.New("export selection must use the documented fields, order, and JSON spelling")
		}
		bound, err := share.PrepareExportSelection(ctx, *db, selection)
		if err != nil {
			return err
		}
		body, err = json.Marshal(exportPrivatePlan{Version: 1, ProducerRevision: revision, Selection: bound})
		if err != nil {
			return errors.New("cannot encode private export plan")
		}
		if err := writeExportPlan(ctx, output, append(body, '\n')); err != nil {
			return err
		}
		return a.writeOutput("Export Prepared", struct {
			Channels int `json:"channels"`
			Messages int `json:"messages"`
		}{len(bound.Channels), len(bound.Messages)}, format, false)
	}
	plan, err := readExportPlan(ctx, input)
	if err != nil {
		return err
	}
	if command == "build" {
		revision, err := a.exportBuildRevision()
		if err != nil {
			return err
		}
		if revision != plan.ProducerRevision {
			return errors.New("private export plan was prepared by a different revision; use that clean build or prepare a new plan")
		}
	}
	// Resolve from the private plan and current archive on every invocation;
	// artifact contents never supply their own expected values.
	projection, err := share.ResolveExportSelection(ctx, *db, plan.Selection)
	if err != nil {
		return err
	}
	var receipt share.ProjectionReceipt
	if command == "build" {
		receipt, err = share.WriteProjection(ctx, output, projection, plan.ProducerRevision)
	} else {
		receipt, err = share.VerifyProjection(ctx, output, projection, plan.ProducerRevision)
	}
	if err != nil {
		return err
	}
	return a.writeOutput("Export Receipt", struct {
		ManifestSHA256 string `json:"manifest_sha256"`
		MessagesSHA256 string `json:"messages_sha256"`
		ManifestBytes  int64  `json:"manifest_bytes"`
		MessagesBytes  int64  `json:"messages_bytes"`
		Rows           int    `json:"rows"`
	}{receipt.ManifestSHA256, receipt.MessagesSHA256, receipt.ManifestBytes, receipt.MessagesBytes, receipt.Rows}, format, false)
}

func (a *App) exportBuildRevision() (string, error) {
	read := a.readBuildInfo
	if read == nil {
		read = debug.ReadBuildInfo
	}
	info, ok := read()
	return cleanExportRevision(info, ok)
}

func cleanExportRevision(info *debug.BuildInfo, ok bool) (string, error) {
	invalid := errors.New("export prepare/build requires clean Git build metadata; commit source changes and rebuild with go build -buildvcs=true ./cmd/slacrawl")
	if !ok || info == nil {
		return "", invalid
	}
	var vcs, revision, modified string
	var vcsCount, revisionCount, modifiedCount int
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs":
			vcs, vcsCount = setting.Value, vcsCount+1
		case "vcs.revision":
			revision, revisionCount = setting.Value, revisionCount+1
		case "vcs.modified":
			modified, modifiedCount = setting.Value, modifiedCount+1
		}
	}
	if vcsCount != 1 || revisionCount != 1 || modifiedCount != 1 || vcs != "git" || modified != "false" || !exportRevisionValid(revision) {
		return "", invalid
	}
	return revision, nil
}

func exportRevisionValid(revision string) bool {
	if len(revision) != 40 {
		return false
	}
	for _, c := range revision {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func readExportPlan(ctx context.Context, path string) (exportPrivatePlan, error) {
	body, err := readExportInput(ctx, path)
	if err != nil {
		return exportPrivatePlan{}, err
	}
	var plan exportPrivatePlan
	if json.Unmarshal(body, &plan) != nil {
		return exportPrivatePlan{}, errors.New("invalid private export plan JSON")
	}
	canonical, err := json.Marshal(plan)
	if err != nil || !bytes.Equal(body, append(canonical, '\n')) || plan.Version != 1 || !exportRevisionValid(plan.ProducerRevision) {
		return exportPrivatePlan{}, errors.New("private export plan must be an unchanged canonical version 1 document")
	}
	return plan, nil
}

func readExportInput(ctx context.Context, path string) ([]byte, error) {
	return readPrivateInput(ctx, path, 0)
}

// A zero limit preserves the existing export input contract. Delivery scope
// documents use a bounded read, including if the file grows after stat.
func readPrivateInput(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("export input must be an existing regular nonsymlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open export input")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("export input identity changed")
	}
	var reader io.Reader = file
	if maxBytes > 0 {
		if opened.Size() > maxBytes {
			return nil, errors.New("private input exceeds its size limit")
		}
		reader = io.LimitReader(file, maxBytes+1)
	}
	body, err := io.ReadAll(reader)
	if maxBytes > 0 && int64(len(body)) > maxBytes {
		return nil, errors.New("private input exceeds its size limit")
	}
	if err != nil {
		return nil, errors.New("cannot read export input")
	}
	after, err := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || int64(len(body)) != after.Size() {
		return nil, errors.New("export input changed during read")
	}
	if err := file.Close(); err != nil {
		return nil, errors.New("cannot close export input")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func writeExportPlan(ctx context.Context, path string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("private export plan destination must be new with an existing parent")
	}
	defer func() { _ = file.Close() }()
	if n, err := file.Write(body); err != nil || n != len(body) {
		return errors.New("cannot write private export plan; incomplete file retained")
	}
	if err := file.Sync(); err != nil {
		return errors.New("cannot sync private export plan; file retained")
	}
	if err := file.Close(); err != nil {
		return errors.New("cannot close private export plan; file retained")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("private export plan retained: %w", err)
	}
	return nil
}
