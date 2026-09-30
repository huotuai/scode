package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scode/internal/agent"
)

// testPNG encodes a real 1x1 PNG (content sniffing needs valid bytes).
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImageBlock(t *testing.T) {
	pngBytes := testPNG(t)
	b, ok := ImageBlock(pngBytes)
	if !ok || b.MimeType != "image/png" {
		t.Fatalf("valid PNG rejected: %+v", b)
	}
	if raw, _ := base64.StdEncoding.DecodeString(b.Data); !bytes.Equal(raw, pngBytes) {
		t.Fatal("base64 round-trip mismatch")
	}
	for name, data := range map[string][]byte{
		"empty":        {},
		"not an image": []byte("hello world, this is plain text"),
		"oversize":     make([]byte, maxImageBytes+1),
	} {
		if _, ok := ImageBlock(data); ok {
			t.Fatalf("%s: should not become an image block", name)
		}
	}
}

func TestScanImagePaths(t *testing.T) {
	dir := t.TempDir()
	pngBytes := testPNG(t)
	img := filepath.Join(dir, "shot.png")
	os.WriteFile(img, pngBytes, 0o644) //nolint:errcheck
	spaced := filepath.Join(dir, "my screenshot.png")
	os.WriteFile(spaced, pngBytes, 0o644) //nolint:errcheck
	notImg := filepath.Join(dir, "notes.png")
	os.WriteFile(notImg, []byte("plain text in a .png"), 0o644) //nolint:errcheck
	wrongExt := filepath.Join(dir, "bitmap.bin")
	os.WriteFile(wrongExt, pngBytes, 0o644) //nolint:errcheck

	// Absolute path, quoted path with spaces, relative path.
	rel, err := filepath.Rel(dir, img)
	if err != nil {
		t.Fatal(err)
	}
	text := "看看这个 " + img + " 和 \"" + spaced + "\" 以及 " + rel
	blocks := ScanImagePaths(text, dir)
	if len(blocks) != 2 {
		t.Fatalf("want 2 image blocks, got %d", len(blocks))
	}

	// Dedupe: the same path twice attaches once.
	if got := ScanImagePaths(img+" "+img, dir); len(got) != 1 {
		t.Fatalf("dedupe: want 1 block, got %d", len(got))
	}

	// Skipped: text content in a .png, PNG content in a non-image
	// extension, a missing file, and ordinary words.
	for _, s := range []string{notImg, wrongExt, filepath.Join(dir, "gone.png"), "hello world"} {
		if got := ScanImagePaths(s, dir); len(got) != 0 {
			t.Fatalf("%q: want 0 blocks, got %d", s, len(got))
		}
	}
}

// End-to-end: a prompt referencing an image file reaches the provider
// as a text part plus an image_url part (pi's attachment flow).
func TestRunAttachesPromptImagePaths(t *testing.T) {
	reqBodies := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqBodies <- string(b)
		chatSSE(w, "got the image", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	pngBytes := testPNG(t)
	img := filepath.Join(app.CWD, "ui.png")
	os.WriteFile(img, pngBytes, 0o644) //nolint:errcheck

	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "描述一下 ui.png 这张图"); err != nil {
		t.Fatal(err)
	}
	for range out { // drain
	}

	body := <-reqBodies
	if !strings.Contains(body, `"image_url"`) {
		t.Fatalf("request carries no image_url part:\n%s", body)
	}
	want := base64.StdEncoding.EncodeToString(pngBytes)
	if !strings.Contains(body, want[:64]) {
		t.Fatalf("request image payload mismatch:\n%s", body)
	}
	if !strings.Contains(body, "描述一下 ui.png 这张图") {
		t.Fatalf("request lost the prompt text:\n%s", body)
	}
}
