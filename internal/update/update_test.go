package update

import (
	"strings"
	"testing"
	"time"

	"scode/internal/i18n"
)

func TestNoticeLocalized(t *testing.T) {
	defer i18n.Set(i18n.En)
	r := CheckResult{Newer: true, Tag: "v0.2.0", Current: "v0.1.0", URL: "https://x"}
	i18n.Set(i18n.En)
	if n := r.Notice(); !strings.Contains(n, "run `scode update`") {
		t.Errorf("en notice: %q", n)
	}
	i18n.Set(i18n.Zh)
	if n := r.Notice(); !strings.Contains(n, "运行 `scode update` 升级") {
		t.Errorf("zh notice: %q", n)
	}
}

func TestNewerSemver(t *testing.T) {
	pub := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		current, tag string
		want         bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"0.1.0", "v0.1.1", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.3.0", "v0.2.0", false},
		{"v1.0.0", "v0.9.9", false},
		{"v0.1.0", "v1.0.0", true},
		// Current is semver but the tag is not: no comparison possible.
		{"v0.1.0", "nightly", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.tag, pub); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.tag, got, c.want)
		}
	}
}

func TestNewerDevBuild(t *testing.T) {
	cases := []struct {
		name    string
		current string
		pub     time.Time
		want    bool
	}{
		// Release published after the build date: outdated.
		{"older build", "ad73b4e+20260201", time.Date(2026, 2, 14, 12, 0, 0, 0, time.UTC), true},
		// Same-day release does not count as newer.
		{"same day", "ad73b4e+20260214", time.Date(2026, 2, 14, 12, 0, 0, 0, time.UTC), false},
		// Release older than the build: current.
		{"newer build", "ad73b4e+20260214", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), false},
		// No date stamp: cannot determine.
		{"bare dev", "dev", time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC), false},
		{"hash only", "ad73b4e", time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC), false},
	}
	for _, c := range cases {
		if got := Newer(c.current, "v0.2.0", c.pub); got != c.want {
			t.Errorf("%s: Newer(%q, v0.2.0, %v) = %v, want %v", c.name, c.current, c.pub, got, c.want)
		}
	}
}

func TestCheckCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := CheckCached(dir, "v0.1.0"); ok {
		t.Fatal("empty cache should report stale")
	}
	pub := time.Now().Add(-time.Hour)
	if err := writeCache(dir, &checkCache{
		CheckedAt:   time.Now(),
		Tag:         "v0.2.0",
		PublishedAt: pub,
		URL:         "https://example.com/r",
	}); err != nil {
		t.Fatal(err)
	}
	res, ok := CheckCached(dir, "v0.1.0")
	if !ok {
		t.Fatal("fresh cache should be usable")
	}
	if !res.Newer || res.Tag != "v0.2.0" || res.URL == "" {
		t.Errorf("unexpected result: %+v", res)
	}
	// A current build reports no update.
	if res, _ := CheckCached(dir, "v0.2.0"); res.Newer {
		t.Error("same tag should not be newer")
	}
}

func TestCheckCacheStale(t *testing.T) {
	dir := t.TempDir()
	if err := writeCache(dir, &checkCache{
		CheckedAt: time.Now().Add(-2 * checkInterval),
		Tag:       "v0.2.0",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := CheckCached(dir, "v0.1.0"); ok {
		t.Fatal("expired cache should report stale")
	}
}
