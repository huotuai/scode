package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// exeName is the binary inside release zips.
func exeName() string {
	if runtime.GOOS == "windows" {
		return "scode.exe"
	}
	return "scode"
}

// SelfUpdate downloads repo's latest release and replaces the running
// executable. The running binary path is renamed aside first (Windows
// allows renaming a locked .exe, not overwriting it); the stale .old
// copy is removed on the next startup via CleanStale.
func SelfUpdate(ctx context.Context, repo, dir, current string, force bool) (*CheckResult, error) {
	res, err := CheckNow(ctx, repo, dir, current)
	if err != nil {
		return res, err
	}
	if !res.Newer && !force {
		return res, nil
	}
	rel, err := FetchLatest(ctx, repo)
	if err != nil {
		return res, err
	}
	asset, err := pickAsset(rel)
	if err != nil {
		return res, err
	}

	tmp, err := os.MkdirTemp("", "scode-update-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	zipPath := filepath.Join(tmp, asset.Name)
	if err := download(ctx, asset.URL, zipPath); err != nil {
		return res, err
	}
	if err := verifyChecksum(ctx, rel, asset.Name, zipPath); err != nil {
		return res, err
	}
	newExe := filepath.Join(tmp, exeName())
	if err := unzip(zipPath, exeName(), newExe); err != nil {
		return res, err
	}
	if err := replaceExecutable(newExe); err != nil {
		return res, err
	}
	return res, nil
}

// pickAsset selects the zip matching this OS/arch (scode-windows-amd64.zip).
func pickAsset(rel *Release) (Asset, error) {
	want := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, want) && strings.HasSuffix(name, ".zip") {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release %s has no zip asset for %s", rel.Tag, want)
}

// download streams url to path with a bounded header wait.
func download(ctx context.Context, url, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "scode-updater")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// verifyChecksum checks the downloaded zip against the release's
// checksums.txt when published; absent checksums are a no-op.
func verifyChecksum(ctx context.Context, rel *Release, assetName, path string) error {
	var sum *Asset
	for i := range rel.Assets {
		if strings.EqualFold(rel.Assets[i].Name, "checksums.txt") {
			sum = &rel.Assets[i]
			break
		}
	}
	if sum == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sum.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", assetName)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 mismatch for %s (got %s, want %s)", assetName, got, want)
	}
	return nil
}

// unzip extracts the single member named exe from the zip.
func unzip(zipPath, exe, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if filepath.Base(f.Name) != exe {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, rc)
		cerr := out.Close()
		if err != nil {
			return err
		}
		return cerr
	}
	return fmt.Errorf("%s not found in %s", exe, zipPath)
}

// replaceExecutable swaps the running binary for newExe. Windows
// refuses to overwrite a running .exe but allows renaming it, so the
// current image is moved to <exe>.old first and deleted best-effort
// (deletion of the in-use image fails until the next launch, where
// CleanStale finishes the job).
func replaceExecutable(newExe string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	old := exe + ".old"
	_ = os.Remove(old) // leftover from a previous update
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("rename running exe: %w", err)
	}
	if err := copyFile(newExe, exe); err != nil {
		// Roll back so the user is not left without a binary.
		_ = os.Rename(old, exe)
		return fmt.Errorf("install new exe: %w", err)
	}
	_ = os.Remove(old) // succeeds on unix; fails while running on Windows
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	cerr := out.Close()
	if err != nil {
		return err
	}
	return cerr
}

// CleanStale removes the <exe>.old left behind by a previous
// SelfUpdate; called at startup (the old image is no longer running).
func CleanStale() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.Remove(exe + ".old")
}
