package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/provider"
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
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", string(SourceAPI), "bot":
		return SourceAPI, nil
	case string(SourceDesktop), "wiretap":
		return SourceDesktop, nil
	case string(SourceMCP), "connector":
		return SourceMCP, nil
	case string(SourceAll), "hybrid":
		return SourceAll, nil
	}
	if name, ok := strings.CutPrefix(normalized, "provider:"); ok && name != "" && !strings.ContainsAny(name, ":/\\\t\r\n ") {
		return Source("provider:" + name), nil
	}
	return "", fmt.Errorf("unsupported source %q: use api, bot, desktop, wiretap, mcp, connector, all, or provider:<name>", value)
}

func ProviderName(source Source) (string, bool) {
	name, ok := strings.CutPrefix(string(source), "provider:")
	return name, ok && name != ""
}

type Options struct {
	Source          Source
	WorkspaceID     string
	WorkspaceSet    bool
	Channels        []string
	ExcludeChannels []string
	Since           string
	Full            bool
	LatestOnly      bool
	Limit           int
	Concurrency     int
	AutoJoin        *bool
	APIURL          string
	HTTPClient      *http.Client
	Logger          *slog.Logger
}

type Summary struct {
	Desktop  slackdesktop.Source `json:"desktop"`
	MCP      *slackmcp.Summary   `json:"mcp,omitempty"`
	Provider *provider.Summary   `json:"provider,omitempty"`
}

func Run(ctx context.Context, cfg config.Config, st *store.Store, opts Options) (Summary, error) {
	return RunWithTokens(ctx, cfg, st, opts, cfg.ResolveTokens())
}

func RunWithTokens(ctx context.Context, cfg config.Config, st *store.Store, opts Options, tokens config.Tokens) (Summary, error) {
	summary := Summary{}
	includeDMs := cfg.IncludeDMsResolved(tokens.User != "")
	apiClient := slackapi.NewWithOptions(tokens, opts.APIURL, opts.HTTPClient).WithIncludeDMs(includeDMs).WithLogger(opts.Logger)

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
		return syncDesktop(ctx, cfg, st, desktopOptionsForSourceAll(opts))
	default:
		name, ok := ProviderName(opts.Source)
		if !ok {
			return summary, errors.New("unsupported source")
		}
		providerConfig, ok := cfg.Provider(name)
		if !ok {
			return summary, fmt.Errorf("provider %q is not configured", name)
		}
		providerSummary, err := provider.Sync(ctx, st, providerConfig, provider.Options{
			WorkspaceID: opts.WorkspaceID, Channels: opts.Channels,
			ExcludeChannels: opts.ExcludeChannels, Since: opts.Since,
			Full: opts.Full, LatestOnly: opts.LatestOnly, Limit: opts.Limit,
			Logger: opts.Logger,
		})
		summary.Provider = &providerSummary
		return summary, err
	}
}

func desktopOptionsForSourceAll(opts Options) Options {
	if !opts.WorkspaceSet {
		opts.WorkspaceID = ""
	}
	return opts
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
