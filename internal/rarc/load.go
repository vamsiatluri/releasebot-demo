package rarc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/awssig"
)

// Fetcher reads the raw document. Overridable in tests.
type Fetcher func(ctx context.Context, rawURL string) ([]byte, error)

// Source describes where the config came from, for /health and the INIT log.
type Source struct {
	URL      string // the URL that was actually read (primary or backup)
	Backup   bool   // true when the primary failed and the backup was used
	Projects int
}

// Loader reads the config ONCE per container and keeps it.
//
// ⚠️ "Once" means once SUCCESSFULLY. A cold start that cannot reach GitHub must
// not brick the container until it is recycled: a failed load is retried on
// the next invocation, and only a good document is cached.
type Loader struct {
	Primary string // ROBBIE_CONFIG_URL
	Backup  string // ROBBIE_CONFIG_URL_BACKUP (optional)
	Fetch   Fetcher

	mu     sync.Mutex
	cfg    *Config
	source Source
}

// Get returns the cached config, loading it if needed.
func (l *Loader) Get(ctx context.Context) (*Config, Source, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cfg != nil {
		return l.cfg, l.source, nil
	}
	fetch := l.Fetch
	if fetch == nil {
		fetch = DefaultFetcher(os.Getenv("GITHUB_TOKEN"))
	}
	if strings.TrimSpace(l.Primary) == "" {
		return nil, Source{}, fmt.Errorf("rarc: ROBBIE_CONFIG_URL is not set")
	}

	cfg, err := loadOne(ctx, fetch, l.Primary)
	src := Source{URL: l.Primary}
	if err != nil && strings.TrimSpace(l.Backup) != "" && l.Backup != l.Primary {
		var berr error
		cfg, berr = loadOne(ctx, fetch, l.Backup)
		if berr != nil {
			return nil, Source{}, fmt.Errorf("rarc: primary failed (%v) and backup failed (%v)", err, berr)
		}
		err, src = nil, Source{URL: l.Backup, Backup: true}
	}
	if err != nil {
		return nil, Source{}, err
	}
	src.Projects = len(cfg.Projects)
	l.cfg, l.source = cfg, src
	return cfg, src, nil
}

func loadOne(ctx context.Context, fetch Fetcher, u string) (*Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := fetch(ctx, u)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// DefaultFetcher understands https://, s3:// and a local path (file:// or bare).
//
// ⛔ The GitHub token is sent ONLY to GitHub's raw-content hosts. The config URL
// is configuration, and a token attached to any https URL would hand a
// credential that can write to repositories to whoever that URL points at.
func DefaultFetcher(githubToken string) Fetcher {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context, rawURL string) ([]byte, error) {
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			return nil, fmt.Errorf("rarc: bad config URL: %w", err)
		}
		switch u.Scheme {
		case "https":
			return fetchHTTPS(ctx, client, u, githubToken)
		case "s3":
			return fetchS3(ctx, client, u)
		case "file", "":
			p := u.Path
			if u.Scheme == "" {
				p = rawURL
			}
			return os.ReadFile(p)
		default:
			return nil, fmt.Errorf("rarc: unsupported config URL scheme %q (use https:// or s3://)", u.Scheme)
		}
	}
}

// IsGitHubRawHost reports whether the GitHub token may be sent to host.
func IsGitHubRawHost(host string) bool {
	switch strings.ToLower(host) {
	case "raw.githubusercontent.com", "raw.github.com":
		return true
	}
	return false
}

func fetchHTTPS(ctx context.Context, c *http.Client, u *url.URL, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if token != "" && IsGitHubRawHost(u.Host) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rarc: fetching %s: %w", redact(u), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rarc: fetching %s: status %d", redact(u), resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// fetchS3 GETs s3://bucket/key with a SigV4-signed request, using the Lambda
// role's credentials from the environment.
func fetchS3(ctx context.Context, c *http.Client, u *url.URL) ([]byte, error) {
	bucket, key := u.Host, strings.TrimPrefix(u.Path, "/")
	if bucket == "" || key == "" {
		return nil, fmt.Errorf("rarc: %s needs a bucket and a key", u.String())
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return nil, fmt.Errorf("rarc: no AWS_REGION for an s3:// config URL")
	}
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return nil, err
	}
	endpoint := (&url.URL{Scheme: "https", Host: bucket + ".s3." + region + ".amazonaws.com",
		Path: "/" + key}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	empty := sha256.Sum256(nil)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(empty[:]))
	awssig.SignGET(req, creds, "s3", region, time.Now())
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rarc: reading %s: %w", u.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rarc: reading %s: status %d", u.String(), resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// redact drops a query string, which is where a signed or tokened URL keeps
// its secret.
func redact(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}
