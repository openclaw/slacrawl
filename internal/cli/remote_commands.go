package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/zalando/go-keyring"
)

func newRemoteClient(cfg config.Config) (*remote.Client, error) {
	token, err := config.ResolveRemoteToken(cfg)
	if err != nil {
		return nil, err
	}
	return remote.NewClientFromConfig(cfg.Remote, remote.Options{TokenProvider: remote.StaticToken(token.Token)})
}

func (a *App) runRemote(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(a.Stdout, "Usage: slacrawl remote <login|whoami|status>\n  login --endpoint URL --archive NAME --github-token-env NAME\n  whoami [--json]\n  status [--json]")
		return err
	}
	if args[0] == "login" {
		return a.runRemoteLogin(ctx, configPath, args[1:], format)
	}
	if args[0] != "whoami" && args[0] != "status" {
		return fmt.Errorf("unknown remote command: %s", args[0])
	}
	fs := flag.NewFlagSet("remote "+args[0], flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "write JSON")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("remote command takes flags only")
	}
	if *jsonOut {
		format = FormatJSON
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if args[0] == "status" {
		return a.runRemoteHealth(ctx, cfg, "status", nil, format)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := newRemoteClient(cfg)
	if err != nil {
		return err
	}
	identity, err := client.Whoami(ctx)
	if err != nil {
		return err
	}
	return a.writeOutput("Remote identity", identity, format, false)
}

// Login exchanges only the explicitly selected local environment credential.
// The returned session goes to the OS keyring; config contains its locator.
func (a *App) runRemoteLogin(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("remote login", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	endpoint := fs.String("endpoint", "", "archive API endpoint")
	archive := fs.String("archive", "", "archive name")
	tokenEnv := fs.String("github-token-env", "", "environment variable holding the local GitHub token")
	timeout := fs.Duration("timeout", 30*time.Second, "login deadline (maximum 2m)")
	jsonOut := fs.Bool("json", false, "write JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("remote login takes flags only")
	}
	if *timeout <= 0 || *timeout > 2*time.Minute {
		return errors.New("remote login --timeout must be between 0 and 2m")
	}
	if strings.TrimSpace(*tokenEnv) == "" {
		return errors.New("remote login requires --github-token-env")
	}
	githubToken := strings.TrimSpace(os.Getenv(*tokenEnv))
	if githubToken == "" {
		return errors.New("selected GitHub token environment variable is empty")
	}
	cfg, err := loadConfigOrDefault(configPath)
	if err != nil {
		return err
	}
	if *endpoint != "" {
		cfg.Remote.Endpoint = *endpoint
	}
	if *archive != "" {
		cfg.Remote.Archive = *archive
	}
	cfg.Remote.Normalize()
	cfg.Remote.Mode = remote.ModeCloud
	if err := cfg.Normalize(); err != nil {
		return err
	}
	if _, err := remoteLoginKeyringAccount(cfg, configPath); err != nil {
		return err
	}
	client, err := remote.NewClient(remote.Options{Endpoint: cfg.Remote.Endpoint, HTTPClient: a.httpClient})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, err := client.LoginWithGitHubToken(ctx, githubToken)
	if err != nil {
		return err
	}
	if result.Status != "complete" || strings.TrimSpace(result.Token) == "" {
		return errors.New("remote login did not return a complete session")
	}
	cfg, err = saveRemoteLogin(cfg, configPath, result.Token)
	if err != nil {
		return err
	}
	auth := cfg.Remote.Auth
	if *jsonOut {
		format = FormatJSON
	}
	return a.writeOutput("Remote login", map[string]any{"config_path": configPath, "endpoint": cfg.Remote.Endpoint, "archive": cfg.Remote.Archive, "login": result.Login, "org": result.Org, "owner": result.Owner, "auth_source": auth.TokenSource, "keyring_service": auth.KeyringService, "keyring_account": auth.KeyringAccount}, format, false)
}

// Match the native reader login binding: endpoint origin/API path, archive and
// selected config are separate credential scopes, even for equal archive names.
func remoteLoginKeyringAccount(cfg config.Config, configPath string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Remote.Endpoint), "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("invalid remote login endpoint")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return "", errors.New("remote login requires HTTPS except for loopback HTTP")
	}
	if u.Path != "" && path.Clean(u.Path) != u.Path {
		return "", errors.New("remote login requires a canonical API path")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	resolved, err := filepath.Abs(configPath)
	if err != nil {
		return "", errors.New("cannot resolve remote configuration path")
	}
	canonical := u.Scheme + "://" + net.JoinHostPort(host, port) + u.EscapedPath()
	binding, _ := json.Marshal([]string{canonical, strings.TrimSpace(cfg.Remote.Archive), resolved})
	return fmt.Sprintf("slacrawl:session:%x", sha256.Sum256(binding)), nil
}

var promoteRemoteLoginConfig = os.Rename

// Stage private config before touching the bound session. A failed promotion
// restores the prior keyring value, or removes only the newly created item.
func saveRemoteLogin(cfg config.Config, configPath, token string) (config.Config, error) {
	account, err := remoteLoginKeyringAccount(cfg, configPath)
	if err != nil {
		return cfg, err
	}
	if info, err := os.Lstat(configPath); err == nil && !info.Mode().IsRegular() {
		return cfg, errors.New("remote login requires a regular configuration file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, errors.New("cannot inspect remote configuration")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		return cfg, errors.New("cannot create remote configuration directory")
	}
	file, err := os.CreateTemp(filepath.Dir(configPath), ".slacrawl-login-*")
	if err != nil {
		return cfg, errors.New("cannot stage remote configuration")
	}
	staged := file.Name()
	defer os.Remove(staged)
	if err := file.Close(); err != nil {
		return cfg, errors.New("cannot close staged remote configuration")
	}
	cfg.Remote.Auth = remote.AuthConfig{TokenSource: "keyring", KeyringService: config.DefaultRemoteTokenKeyringService, KeyringAccount: account}
	if err := cfg.Save(staged); err != nil {
		return cfg, errors.New("cannot stage remote configuration")
	}
	service := cfg.Remote.Auth.KeyringService
	previous, previousErr := keyring.Get(service, account)
	if previousErr != nil && !errors.Is(previousErr, keyring.ErrNotFound) {
		return cfg, errors.New("cannot inspect bound remote session")
	}
	if _, err := config.StoreRemoteToken(cfg, token); err != nil {
		return cfg, errors.New("cannot store bound remote session")
	}
	if err := promoteRemoteLoginConfig(staged, configPath); err != nil {
		var rollback error
		if previousErr == nil {
			rollback = keyring.Set(service, account, previous)
		} else {
			rollback = keyring.Delete(service, account)
		}
		if rollback != nil {
			return cfg, errors.New("remote configuration promotion failed and prior session could not be restored")
		}
		return cfg, errors.New("cannot promote remote configuration; prior session restored")
	}
	return cfg, nil
}
