// Package update implements scode's self-update plumbing: GitHub
// release lookups, new-version comparison (semver tags and dev builds
// stamped hash+date), a 24h on-disk check cache, and the `scode update`
// self-replacement of the running executable.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is the baked-in GitHub "org/repo" release source;
// SCODE_UPDATE_REPO or settings.json updateRepo override it.
const DefaultRepo = "huotuai/scode"

// checkInterval throttles background release lookups.
const checkInterval = 24 * time.Hour

// httpTimeout bounds release API and asset downloads' metadata leg.
const httpTimeout = 10 * time.Second

// Release describes the latest published GitHub release.
type Release struct {
	Tag         string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset is one downloadable release file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// checkCache is the persisted result of the last release lookup
// (~/.scode/update-check.json), so interactive sessions never hit the
// network synchronously.
type checkCache struct {
	CheckedAt   time.Time `json:"checkedAt"`
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"publishedAt"`
	URL         string    `json:"url"`
}

// cachePath locates the check cache under dir (~/.scode).
func cachePath(dir string) string { return filepath.Join(dir, "update-check.json") }

// readCache loads the cache; a missing or corrupt file yields nil.
func readCache(dir string) *checkCache {
	data, err := os.ReadFile(cachePath(dir))
	if err != nil {
		return nil
	}
	var c checkCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

// writeCache persists the lookup result (best effort).
func writeCache(dir string, c *checkCache) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cachePath(dir), append(b, '\n'), 0o644)
}

// FetchLatest queries GitHub for repo's newest release.
func FetchLatest(ctx context.Context, repo string) (*Release, error) {
	if repo == "" {
		return nil, fmt.Errorf("update source not configured (set updateRepo in settings.json or SCODE_UPDATE_REPO, e.g. \"org/scode\")")
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "scode-updater")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases: %s", resp.Status)
	}
	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("github releases: %w", err)
	}
	if r.Tag == "" {
		return nil, fmt.Errorf("github releases: empty tag")
	}
	return &r, nil
}

// CheckResult reports whether a newer release exists.
type CheckResult struct {
	Newer   bool
	Tag     string
	URL     string
	Current string
}

// Notice is the one-line hint printed to users when Newer is true.
func (r CheckResult) Notice() string {
	return fmt.Sprintf("scode 有新版本 %s（当前 %s）— 运行 `scode update` 升级（%s）",
		r.Tag, r.Current, r.URL)
}

// CheckNow queries the release source and compares against current,
// refreshing the cache regardless of its age.
func CheckNow(ctx context.Context, repo, dir, current string) (*CheckResult, error) {
	rel, err := FetchLatest(ctx, repo)
	if err != nil {
		return nil, err
	}
	_ = writeCache(dir, &checkCache{
		CheckedAt:   time.Now(),
		Tag:         rel.Tag,
		PublishedAt: rel.PublishedAt,
		URL:         rel.HTMLURL,
	})
	return &CheckResult{
		Newer:   Newer(current, rel.Tag, rel.PublishedAt),
		Tag:     rel.Tag,
		URL:     rel.HTMLURL,
		Current: current,
	}, nil
}

// CheckCached returns the comparison from the on-disk cache when it is
// still fresh; ok=false means a background refresh is due.
func CheckCached(dir, current string) (res CheckResult, ok bool) {
	c := readCache(dir)
	if c == nil || c.Tag == "" || time.Since(c.CheckedAt) > checkInterval {
		return CheckResult{}, false
	}
	return CheckResult{
		Newer:   Newer(current, c.Tag, c.PublishedAt),
		Tag:     c.Tag,
		URL:     c.URL,
		Current: current,
	}, true
}

// RefreshCache performs a fire-and-forget lookup that only updates the
// on-disk cache; the caller decides how to surface the result. Errors
// are swallowed (offline machines must not nag).
func RefreshCache(repo, dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	rel, err := FetchLatest(ctx, repo)
	if err != nil {
		return
	}
	_ = writeCache(dir, &checkCache{
		CheckedAt:   time.Now(),
		Tag:         rel.Tag,
		PublishedAt: rel.PublishedAt,
		URL:         rel.HTMLURL,
	})
}

// ---------------------------------------------------------------------------
// version comparison
//
// Release tags are semver-ish ("v0.2.0"); build.bat stamps dev binaries
// "<githash>+<yyyymmdd>". Semver beats date comparison when both parse;
// a date-stamped dev build is considered outdated once a release is
// published after its build date.
// ---------------------------------------------------------------------------

var (
	semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)
	dateRe   = regexp.MustCompile(`\+(\d{8})$`)
)

// Newer reports whether the release (tag, publishedAt) is newer than
// the running build (current, from -ldflags -X main.version).
func Newer(current, tag string, publishedAt time.Time) bool {
	if cur, ok := parseSemver(current); ok {
		if lat, ok := parseSemver(tag); ok {
			return compareSemver(lat, cur) > 0
		}
		return false
	}
	// Dev build: compare the +yyyymmdd stamp against the publish date.
	if d, ok := buildDate(current); ok && !publishedAt.IsZero() {
		return publishedAt.After(d.Add(24 * time.Hour)) // same-day releases don't count
	}
	return false
}

// parseSemver extracts major.minor.patch from "v1.2.3" / "1.2.3-rc".
func parseSemver(v string) ([3]int, bool) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func compareSemver(a, b [3]int) int {
	for i := range a {
		switch {
		case a[i] > b[i]:
			return 1
		case a[i] < b[i]:
			return -1
		}
	}
	return 0
}

// buildDate extracts the +yyyymmdd build stamp (build.bat/build.sh).
func buildDate(v string) (time.Time, bool) {
	m := dateRe.FindStringSubmatch(v)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102", m[1])
	return t, err == nil
}
