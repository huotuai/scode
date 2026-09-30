package tui

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
	"scode/internal/llm"
)

// tinyPNG encodes a real 1x1 PNG (content sniffing needs valid bytes).
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// dib24 builds a raw 1x1 24bpp DIB (BITMAPINFOHEADER + one padded
// pixel row), the shape CF_DIB takes on the Windows clipboard.
func dib24() []byte {
	var buf bytes.Buffer
	row := []byte{0x11, 0x22, 0x33, 0x00}                     // BGR + pad to 4 bytes
	binary.Write(&buf, binary.LittleEndian, uint32(40))       // header size //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, int32(1))         // width //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, int32(1))         // height //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, uint16(1))        // planes //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, uint16(24))       // bpp //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, uint32(0))        // BI_RGB //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, uint32(len(row))) // sizeImage //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, make([]byte, 16)) // ppm + colors //nolint:errcheck
	buf.Write(row)
	return buf.Bytes()
}

func TestDibToPNG(t *testing.T) {
	out, err := dibToPNG(dib24())
	if err != nil {
		t.Fatal(err)
	}
	if mime := llm.DetectImageMIME(out); mime != "image/png" {
		t.Fatalf("converted payload sniffs as %q, not image/png", mime)
	}
	for _, bad := range [][]byte{{}, make([]byte, 10), append([]byte{255, 255, 255, 255}, make([]byte, 40)...)} {
		if _, err := dibToPNG(bad); err == nil {
			t.Fatalf("len=%d: expected an error", len(bad))
		}
	}
}

// put32/put16 write little-endian fields into a header under
// construction.
func put32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func put16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }

// dib32 builds a 2x2 32bpp DIB. Rows: top red/green, bottom blue/white
// (BGRA pixels). With topDown the rows are written top-first under a
// negative height instead of bottom-up. headerSize/compression/amask
// select the real-world clipboard shapes: V5 screenshots carry
// BI_BITFIELDS with the alpha mask GDI leaves behind (often 0), and
// CF_DIB snapshots trail a 40-byte header with a mask block.
func dib32(headerSize int, compression uint32, amask uint32, topDown bool) []byte {
	hdr := make([]byte, headerSize)
	put32(hdr, 0, uint32(headerSize))
	put32(hdr, 4, 2)
	h := int32(2)
	if topDown {
		h = -2
	}
	put32(hdr, 8, uint32(h))
	put16(hdr, 12, 1)
	put16(hdr, 14, 32)
	put32(hdr, 16, compression)
	if headerSize >= 52 {
		put32(hdr, 40, 0xFF0000)
		put32(hdr, 44, 0xFF00)
		put32(hdr, 48, 0xFF)
		if headerSize >= 56 {
			put32(hdr, 52, amask)
		}
	}
	var buf bytes.Buffer
	buf.Write(hdr)
	if headerSize == 40 && (compression == 3 || compression == 6) {
		// Trailing mask block: 3 dwords (RGB), plus alpha for
		// BI_ALPHABITFIELDS.
		masks := []uint32{0xFF0000, 0xFF00, 0xFF}
		if compression == 6 {
			masks = append(masks, amask)
		}
		for _, v := range masks {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], v)
			buf.Write(b[:])
		}
	}
	// Pixel alpha byte: 0 when no alpha mask applies (the decoder must
	// force opaque), 255 when the mask honors it.
	var a byte
	if amask != 0 {
		a = 255
	}
	px := func(rows ...[4]byte) {
		for _, p := range rows { // each pixel is BGRA
			p[3] = a
			buf.Write(p[:])
		}
	}
	top := [][4]byte{{0, 0, 255, 0}, {0, 255, 0, 0}}        // red, green (BGRA)
	bottom := [][4]byte{{255, 0, 0, 0}, {255, 255, 255, 0}} // blue, white
	if topDown {
		px(top...)
		px(bottom...)
	} else {
		px(bottom...)
		px(top...)
	}
	return buf.Bytes()
}

// dib16 builds a 1x2 16bpp 5:6:5 BI_BITFIELDS DIB (top red, bottom
// green), masks trailing the 40-byte header.
func dib16() []byte {
	hdr := make([]byte, 40)
	put32(hdr, 0, 40)
	put32(hdr, 4, 1)
	put32(hdr, 8, 2)
	put16(hdr, 12, 1)
	put16(hdr, 14, 16)
	put32(hdr, 16, 3)
	var buf bytes.Buffer
	buf.Write(hdr)
	for _, v := range []uint32{0xF800, 0x07E0, 0x001F} {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		buf.Write(b[:])
	}
	// bottom-up rows, each one pixel padded to 4 bytes: green row then red
	var row [4]byte
	binary.LittleEndian.PutUint16(row[0:], 0x07E0) // green
	buf.Write(row[:])
	binary.LittleEndian.PutUint16(row[0:], 0xF800) // red
	buf.Write(row[:])
	return buf.Bytes()
}

// dib8 builds a 2x1 8bpp paletted DIB (palette: blue, yellow) — the
// shape that still goes through the x/image/bmp wrap.
func dib8() []byte {
	hdr := make([]byte, 40)
	put32(hdr, 0, 40)
	put32(hdr, 4, 2)
	put32(hdr, 8, 1)
	put16(hdr, 12, 1)
	put16(hdr, 14, 8)
	put32(hdr, 16, 0)
	put32(hdr, 32, 2) // colorsUsed
	var buf bytes.Buffer
	buf.Write(hdr)
	for _, c := range [][4]byte{ // BGRA palette entries
		{255, 0, 0, 0}, {0, 255, 255, 0}, // blue, yellow
	} {
		buf.Write(c[:])
	}
	buf.Write([]byte{0, 1, 0, 0}) // pixels: blue, yellow; row padded to 4
	return buf.Bytes()
}

// pngPixel decodes a PNG back and reads one pixel as NRGBA (opaque
// conversions are exact).
func pngPixel(t *testing.T, pngBytes []byte, x, y int) color.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("re-decode png: %v", err)
	}
	n, ok := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	if !ok {
		t.Fatalf("pixel(%d,%d) converted to %T", x, y, n)
	}
	return n
}

// The real-world screenshot shapes that x/image/bmp rejects —
// BI_BITFIELDS with a non-default alpha mask, and mask blocks between a
// 40-byte header and the pixels — must decode, with correct colors and
// opaque alpha when no alpha mask is present.
func TestDibToPNGScreenshotShapes(t *testing.T) {
	cases := []struct {
		name string
		dib  []byte
	}{
		{"v5-bitfields-alpha0", dib32(124, 3, 0, false)}, // GDI screenshots (the field failure)
		{"v5-bitfields-alpha", dib32(124, 3, 0xFF000000, false)},
		{"v4-bitfields-alpha0", dib32(108, 3, 0, false)},
		{"v5-birgb", dib32(124, 0, 0, false)},
		{"info-bitfields-trailing", dib32(40, 3, 0, false)}, // CF_DIB snapshot
		{"info-alphabitfields", dib32(40, 6, 0xFF000000, false)},
		{"v5-bitfields-topdown", dib32(124, 3, 0, true)}, // negative height
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := dibToPNG(c.dib)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				x, y       int
				r, g, b, a uint8
			}{
				{0, 0, 255, 0, 0, 255},     // top-left: red
				{1, 0, 0, 255, 0, 255},     // top-right: green
				{0, 1, 0, 0, 255, 255},     // bottom-left: blue
				{1, 1, 255, 255, 255, 255}, // bottom-right: white
			} {
				if got := pngPixel(t, out, c.x, c.y); got.R != c.r || got.G != c.g || got.B != c.b || got.A != c.a {
					t.Fatalf("pixel(%d,%d) = %v, want (%d,%d,%d,%d)", c.x, c.y, got, c.r, c.g, c.b, c.a)
				}
			}
		})
	}
}

// 16bpp 5:6:5 masks scale channels to full range; 8bpp palettes still
// decode through the BMP wrap.
func TestDibToPNG16And8bpp(t *testing.T) {
	p16 := mustPNG(t, dib16())
	if got := pngPixel(t, p16, 0, 0); got.R != 255 || got.G != 0 || got.B != 0 {
		t.Fatalf("16bpp red = %v", got)
	}
	if got := pngPixel(t, p16, 0, 1); got.R != 0 || got.G != 255 || got.B != 0 {
		t.Fatalf("16bpp green = %v", got)
	}

	p8 := mustPNG(t, dib8())
	if got := pngPixel(t, p8, 0, 0); got.B != 255 || got.R != 0 || got.G != 0 {
		t.Fatalf("8bpp palette blue = %v", got)
	}
	if got := pngPixel(t, p8, 1, 0); got.R != 255 || got.G != 255 || got.B != 0 {
		t.Fatalf("8bpp palette yellow = %v", got)
	}
}

func mustPNG(t *testing.T, dib []byte) []byte {
	t.Helper()
	out, err := dibToPNG(dib)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Unsupported compression (RLE) errors instead of decoding garbage.
func TestDibToPNGRejectsRLE(t *testing.T) {
	dib := dib32(40, 1, 0, false)
	if _, err := dibToPNG(dib); err == nil {
		t.Fatal("RLE-compressed DIB decoded without error")
	}
}

func TestSplitReferenced(t *testing.T) {
	c1 := clipImage{id: 1, block: llm.Block{Kind: llm.BlockImage, MimeType: "image/png", Data: "QQ=="}}
	c2 := clipImage{id: 2, block: llm.Block{Kind: llm.BlockImage, MimeType: "image/png", Data: "Qg=="}}
	kept, dropped := splitReferenced("看这个 [图片#2] 怎么样", []clipImage{c1, c2})
	if len(kept) != 1 || kept[0].id != 2 {
		t.Fatalf("kept = %+v", kept)
	}
	if len(dropped) != 1 || dropped[0].id != 1 {
		t.Fatalf("dropped = %+v", dropped)
	}
	blocks := imageBlocks(kept)
	if len(blocks) != 1 || blocks[0].Kind != llm.BlockImage {
		t.Fatalf("blocks = %+v", blocks)
	}
	if got := stripPlaceholders("a [图片#1] b [图片#2]", []clipImage{c1, c2}); got != "a  b " {
		t.Fatalf("strip = %q", got)
	}
}

// ctrl+v with an image on the clipboard attaches it and echoes the
// placeholder; with no image it falls through to text paste; an
// unusable payload is consumed with an error notice.
func TestAttachClipboardImage(t *testing.T) {
	app := setupPaletteApp(t)
	pngBytes := tinyPNG(t)

	stubClipboard(t, func() ([]byte, error) { return pngBytes, nil })

	m := newModel(app, make(chan any, 16))
	if !m.attachClipboardImage() {
		t.Fatal("image clipboard not consumed")
	}
	if got := m.input.Value(); got != "[图片#1]" {
		t.Fatalf("input = %q", got)
	}
	if len(m.clipImgs) != 1 || m.clipImgs[0].block.Kind != llm.BlockImage {
		t.Fatalf("clipImgs = %+v", m.clipImgs)
	}

	// Clipboard without an image: keystroke falls through.
	stubClipboard(t, func() ([]byte, error) { return nil, errNoClipboardImage })
	if m.attachClipboardImage() {
		t.Fatal("no-image clipboard should fall through")
	}

	// Unusable payload: consumed with an error notice, no attachment.
	stubClipboard(t, func() ([]byte, error) { return []byte("not an image at all"), nil })
	m2 := newModel(app, make(chan any, 16))
	if !m2.attachClipboardImage() {
		t.Fatal("unusable image should still be consumed")
	}
	if len(m2.clipImgs) != 0 {
		t.Fatalf("unusable image attached: %+v", m2.clipImgs)
	}
}

// End-to-end: paste a screenshot (stubbed clipboard), type text, Enter —
// the provider request carries the text and the image part.
func TestProgramClipboardImage(t *testing.T) {
	reqBodies := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqBodies <- string(b)
		chatSSEBlob(w, "ok")
	}))
	defer srv.Close()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settingsJSON := `{"defaultProvider":"openai-compat","sandbox":{"mode":"danger-full-access"},"providers":{"openai-compat":{"apiKey":"k","baseUrl":"` + srv.URL + `","model":"m"}}}`
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(settingsJSON), 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	ui := make(chan any, 256)
	app, err := cli.Setup(cli.Options{Approver: newApprover(ui)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	orig := clipboardImageFn
	t.Cleanup(func() { clipboardImageFn = orig })
	pngBytes := tinyPNG(t)
	clipboardImageFn = func() ([]byte, error) { return pngBytes, nil }

	pr, pw := io.Pipe()
	out := &syncWriter{}
	p := tea.NewProgram(newModel(app, ui),
		tea.WithInput(pr),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 30),
		tea.WithoutSignals(),
	)
	runDone := make(chan error, 1)
	go func() { _, err := p.Run(); runDone <- err }()

	if err := (scriptWriter{pw}).write("\x16"); err != nil { // ctrl+v
		t.Fatal(err)
	}
	if err := (scriptWriter{pw}).write("这是什么\r"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var body string
	for body == "" {
		if time.Now().After(deadline) {
			t.Fatalf("provider never called; output:\n%s", stripANSI(out.String()))
		}
		select {
		case body = <-reqBodies:
		case err := <-runDone:
			t.Fatalf("program exited early: %v; output:\n%s", err, stripANSI(out.String()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, want := range []string{`"image_url"`, "这是什么"} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %q; body:\n%s", want, body)
		}
	}
	time.Sleep(200 * time.Millisecond)
	quitProgram(t, pw, runDone)
}

// A truncated BI_BITFIELDS/BI_ALPHABITFIELDS payload must error, not
// panic: the mask reads used to run before the truncation guard, and a
// 40-55 byte DIB crashed the whole TUI on the UI goroutine.
func TestDecodeDIBTruncatedBitfields(t *testing.T) {
	mk := func(headerSize int, compression uint32, size int) []byte {
		b := make([]byte, size)
		binary.LittleEndian.PutUint32(b[0:], uint32(headerSize))
		binary.LittleEndian.PutUint32(b[4:], 2)   // width
		binary.LittleEndian.PutUint32(b[8:], 2)   // height
		binary.LittleEndian.PutUint16(b[12:], 1)  // planes
		binary.LittleEndian.PutUint16(b[14:], 32) // bpp
		binary.LittleEndian.PutUint32(b[16:], compression)
		return b
	}
	for _, c := range []struct {
		headerSize  int
		compression uint32
		size        int
	}{
		{40, 3, 44}, // RGB masks need bytes [40,52)
		{40, 3, 51},
		{40, 6, 44}, // alpha mask needs bytes [40,56)
		{40, 6, 55},
		{52, 6, 54}, // 52-byte header + alpha mask needs [40,56)
	} {
		if _, err := decodeDIB(mk(c.headerSize, c.compression, c.size)); err == nil {
			t.Fatalf("headerSize=%d compression=%d size=%d: expected truncation error", c.headerSize, c.compression, c.size)
		}
	}
}
