package cli

import (
	"github.com/openclaw/crawlkit/control"
	"github.com/openclaw/slacrawl/internal/config"
)

func controlManifest(configPath string) control.Manifest {
	defaults := config.Default()
	manifest := control.NewManifest("slacrawl", "Slack Crawl", "slacrawl")
	manifest.Description = "Local-first Slack archive crawler."
	manifest.Branding = control.Branding{
		SymbolName:       "number",
		AccentColor:      "#4a154b",
		BundleIdentifier: "com.tinyspeck.slackmacgap",
	}
	manifest.Paths = control.Paths{
		DefaultConfig:   configPath,
		ConfigEnv:       "SLACRAWL_CONFIG",
		DefaultDatabase: defaults.DBPath,
		DefaultCache:    defaults.CacheDir,
		DefaultLogs:     defaults.LogDir,
		DefaultShare:    defaults.Share.RepoPath,
	}
	manifest.Capabilities = []string{"metadata", "doctor", "status", "sync", "search", "watch", "tap", "tui", "sql", "git-share"}
	manifest.Privacy = control.Privacy{
		ContainsPrivateMessages: true,
		ExportsSecrets:          false,
		LocalOnlyScopes:         []string{"slack", "desktop-cache", "sqlite", "git-share"},
	}
	manifest.Commands = map[string]control.Command{
		"doctor":      {Title: "Doctor", Argv: []string{"slacrawl", "--json", "doctor"}, JSON: true},
		"status":      {Title: "Status", Argv: []string{"slacrawl", "--json", "status"}, JSON: true},
		"sync":        {Title: "Sync", Argv: []string{"slacrawl", "--json", "sync", "--source", "all", "--latest-only"}, JSON: true, Mutates: true},
		"search":      {Title: "Search", Argv: []string{"slacrawl", "--json", "search"}, JSON: true},
		"tap":         {Title: "Import desktop cache", Argv: []string{"slacrawl", "--json", "sync", "--source", "desktop"}, JSON: true, Mutates: true},
		"tui":         {Title: "Terminal browser", Argv: []string{"slacrawl", "tui"}},
		"tui-json":    {Title: "Terminal browser rows", Argv: []string{"slacrawl", "tui", "--json"}, JSON: true},
		"publish":     {Title: "Publish share", Argv: []string{"slacrawl", "--json", "publish"}, JSON: true, Mutates: true},
		"subscribe":   {Title: "Subscribe share", Argv: []string{"slacrawl", "--json", "subscribe"}, JSON: true, Mutates: true},
		"update":      {Title: "Update share", Argv: []string{"slacrawl", "--json", "update"}, JSON: true, Mutates: true},
		"legacy-json": {Title: "Legacy JSON flag", Argv: []string{"slacrawl", "--json"}, JSON: true, Legacy: true},
	}
	return manifest
}
