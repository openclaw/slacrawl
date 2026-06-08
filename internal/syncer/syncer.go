package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/slackapi"
	"github.com/openclaw/slacrawl/internal/slackdesktop"
	"github.com/openclaw/slacrawl/internal/slackmcp"
	"github.com/openclaw/slacrawl/internal/store"
)

type Source string

const (
	SourceAPI     Source = "api"
	SourceDesktop Source = "desktop"
	SourceMCP     Source = "mcp"
	SourceAll     Source = "all"
)

func ParseSource(value string) (Source, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(SourceAPI), "bot":
		return SourceAPI, nil
	case string(SourceDesktop), "wiretap":
		return SourceDesktop, nil
	case string(SourceMCP), "connector":
		return SourceMCP, nil
	case string(SourceAll), "hybrid":
		return SourceAll, nil
	default:
		return "", fmt.Errorf("unsupported source %q: use api, bot, desktop, wiretap, mcp, connector, or all", value)
	}
}

type Options struct {
	Source          Source
	WorkspaceID     string
	Channels        []string
	ExcludeChannels []string
	Since           string
	Full            bool
	LatestOnly      bool
	Concurrency     int
	AutoJoin        *bool
	Logger          *slog.Logger
}

type Summary struct {
	Desktop slackdesktop.Source `json:"desktop"`
	MCP     *slackmcp.Summary   `json:"mcp,omitempty"`
}

func Run(ctx context.Context, cfg config.Config, st *store.Store, opts Options) (Summary, error) {
	return RunWithTokens(ctx, cfg, st, opts, cfg.ResolveTokens())
}

func RunWithTokens(ctx context.Context, cfg config.Config, st *store.Store, opts Options, tokens config.Tokens) (Summary, error) {
	summary := Summary{}
	includeDMs := cfg.IncludeDMsResolved(tokens.User != "")
	apiClient := slackapi.New(tokens).WithIncludeDMs(includeDMs).WithLogger(opts.Logger)

	switch opts.Source {
	case SourceAPI:
		return summary, apiClient.Sync(ctx, st, slackapi.SyncOptions{
			WorkspaceID:     opts.WorkspaceID,
			Channels:        opts.Channels,
			ExcludeChannels: opts.ExcludeChannels,
			Since:           opts.Since,
			Full:            opts.Full,
			LatestOnly:      opts.LatestOnly,
			Concurrency:     opts.Concurrency,
			AutoJoin:        opts.AutoJoin,
		})
	case SourceDesktop:
		return syncDesktop(ctx, cfg, st, opts)
	case SourceMCP:
		if !cfg.Slack.MCP.Enabled {
			return summary, errors.New("slack MCP source is disabled in config")
		}
		mcpSummary, err := slackmcp.Sync(ctx, st, slackmcp.Options{
			WorkspaceID:     opts.WorkspaceID,
			Channels:        opts.Channels,
			ExcludeChannels: opts.ExcludeChannels,
			Since:           opts.Since,
			Full:            opts.Full,
			LatestOnly:      opts.LatestOnly,
			Logger:          opts.Logger,
			Config:          cfg.Slack.MCP,
		})
		summary.MCP = &mcpSummary
		return summary, err
	case SourceAll:
		if err := apiClient.Sync(ctx, st, slackapi.SyncOptions{
			WorkspaceID:     opts.WorkspaceID,
			Channels:        opts.Channels,
			ExcludeChannels: opts.ExcludeChannels,
			Since:           opts.Since,
			Full:            opts.Full,
			LatestOnly:      opts.LatestOnly,
			Concurrency:     opts.Concurrency,
			AutoJoin:        opts.AutoJoin,
		}); err != nil {
			return summary, err
		}
		return syncDesktop(ctx, cfg, st, opts)
	default:
		return summary, errors.New("unsupported source")
	}
}

func syncDesktop(ctx context.Context, cfg config.Config, st *store.Store, opts Options) (Summary, error) {
	if !cfg.Slack.Desktop.Enabled {
		return Summary{Desktop: slackdesktop.Source{Path: cfg.Slack.Desktop.Path, Available: false}}, nil
	}
	source, err := slackdesktop.Ingest(ctx, st, cfg.Slack.Desktop.Path, slackdesktop.IngestOptions{
		WorkspaceID:     opts.WorkspaceID,
		Channels:        opts.Channels,
		ExcludeChannels: opts.ExcludeChannels,
	})
	if err != nil {
		return Summary{}, err
	}
	return Summary{Desktop: source}, nil
}
