package tui

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/image/bmp"

	"scode/internal/cli"
	"scode/internal/llm"
)

// Clipboard image attachments (screenshots, copied image files).
//
// ctrl+v first probes the OS clipboard for an image: a bitmap (Windows
// CF_DIBV5/CF_DIB — what Win+Shift+S and PrtScn leave behind), a raw
// PNG ("PNG" format), or an Explorer-copied image FILE (CF_HDROP). A
// hit becomes a pending attachment echoed into the input as a [图片#N]
// placeholder; a miss falls through to the textarea's normal text
// paste. Deleting the placeholder drops the attachment.

// errNoClipboardImage means the clipboard holds no usable image (or is
// busy); the caller falls back to text paste.
var errNoClipboardImage = errors.New("no image on the clipboard")

// Clipboard seams (stubbed in tests). clipboardImageFn backs the
// trigger-key on-demand read and the armed watch loop; the text
// writer backs the selection-copy path.
var (
	clipboardImageFn     = readClipboardImage
	writeClipboardTextFn = writeClipboardText
)

// clipImage is one pending clipboard attachment awaiting submission.
type clipImage struct {
	id    int
	block llm.Block // ready-made image block
	size  int       // raw payload bytes, for the notice
}

func (c clipImage) placeholder() string { return fmt.Sprintf("[图片#%d]", c.id) }

// attachImage queues a validated image payload as a pending attachment
// and echoes its placeholder into the composer. name labels the source
// in the notice (clipboard / file name).
func (m *model) attachImage(data []byte, name string) bool {
	block, ok := cli.ImageBlock(data)
	if !ok {
		return false
	}
	m.imgSeq++
	c := clipImage{id: m.imgSeq, block: block, size: len(data)}
	m.clipImgs = append(m.clipImgs, c)
	m.input.InsertString(c.placeholder())
	m.appendBlock(dimStyle.Render(fmt.Sprintf("(已附加 %s %s，%d 字节 — 随下条消息发送；删除占位符可取消)", name, c.placeholder(), c.size)))
	m.resize()
	return true
}

// attachImageFile reads an image file into a pending attachment.
func (m *model) attachImageFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return m.attachImage(data, filepath.Base(path))
}

// attachClipboardImage probes the clipboard for an image ONCE (the
// alt+v / ctrl+v / ctrl+shift+v trigger) and, on success, queues it
// and echoes its placeholder into the input. Nothing runs between
// presses unless the watch loop below is armed. Reports whether the
// keystroke was consumed; false means fall through to text paste.
func (m *model) attachClipboardImage() bool {
	data, err := clipboardImageFn()
	if err != nil {
		return false
	}
	if !m.attachImage(data, "剪贴板图片") {
		m.appendBlock(errStyle.Render(fmt.Sprintf("clipboard image rejected: not a supported image or over %d bytes (%d bytes)", 10<<20, len(data))))
		return true
	}
	return true
}

// Clipboard watch loop (armed by a trigger key that found no image).
//
// The one-shot probe misses the common "press the trigger FIRST, take the
// screenshot after" flow, so a miss arms a bounded poll: every
// clipLoopInterval the clipboard is re-probed until an image appears
// (attach + stop), the clipLoopWindow deadline passes, the message is
// submitted, or the next ctrl+v re-arms the loop. clipLoopGen
// invalidates stale ticks — a superseded or stopped loop goes quiet
// instead of probing on. At idle the clipboard API is never touched.
const (
	clipLoopInterval = 150 * time.Millisecond
	clipLoopWindow   = 15 * time.Second
)

// clipLoopTickMsg is one poll of the armed watch; gen ties the tick to
// the arming press.
type clipLoopTickMsg struct{ gen int }

// armClipLoop (re)starts the bounded clipboard watch.
func (m *model) armClipLoop() tea.Cmd {
	m.clipLoopGen++
	m.clipLoopUntil = time.Now().Add(clipLoopWindow)
	return m.clipLoopTick()
}

// disarmClipLoop stops the watch (image attached, submit, expiry).
func (m *model) disarmClipLoop() {
	m.clipLoopGen++
	m.clipLoopUntil = time.Time{}
}

// clipLoopTick schedules the next poll of the armed watch.
func (m *model) clipLoopTick() tea.Cmd {
	gen := m.clipLoopGen
	return tea.Tick(clipLoopInterval, func(time.Time) tea.Msg { return clipLoopTickMsg{gen: gen} })
}

// onClipLoopTick handles one poll: stale ticks die silently, an
// expired window reports and stops, an image hit attaches and stops,
// anything else schedules the next poll.
func (m *model) onClipLoopTick(msg clipLoopTickMsg) tea.Cmd {
	if msg.gen != m.clipLoopGen || m.clipLoopUntil.IsZero() {
		return nil // stale tick from a superseded/stopped loop
	}
	if !time.Now().Before(m.clipLoopUntil) {
		m.disarmClipLoop()
		m.appendBlock(dimStyle.Render("(剪贴板监控超时：未检测到图片，按 alt+v / ctrl+v 重新监控)"))
		m.resize()
		return nil
	}
	if data, err := clipboardImageFn(); err == nil {
		m.disarmClipLoop()
		if !m.attachImage(data, "剪贴板图片") {
			m.appendBlock(errStyle.Render(fmt.Sprintf("clipboard image rejected: not a supported image or over %d bytes (%d bytes)", 10<<20, len(data))))
		}
		return nil
	}
	return m.clipLoopTick()
}

// handlePaste intercepts a bracketed paste: image file paths inside the
// pasted text (Explorer's copy-image-file, terminal drag-drop, IDE temp
// paths — terminals paste those as quoted text) become attachments
// immediately and drop out of the text; any remainder pastes normally.
// Reports whether the paste was fully consumed (callers stop here and
// must not feed the original text to the textarea).
func (m *model) handlePaste(content string) bool {
	if m.app == nil || !m.app.Model.Caps.ImageInput {
		return false // unsupported model: plain text paste, submit-time scan stays as is
	}
	refs := cli.ExtractImageRefs(content, m.app.CWD)
	if len(refs) == 0 {
		return false
	}
	attached := 0
	for _, r := range refs {
		if m.attachImageFile(r.Path) {
			content = stripPasteToken(content, r.Token)
			attached++
		}
	}
	if attached == 0 {
		return false
	}
	if rest := strings.TrimSpace(content); rest != "" {
		m.input.InsertString(rest + " ")
		m.updatePalette()
		m.resize()
	}
	return true
}

// stripPasteToken removes one path token (bare or quoted) from pasted
// text.
func stripPasteToken(content, token string) string {
	for _, form := range []string{`"` + token + `"`, "'" + token + "'", token} {
		content = strings.ReplaceAll(content, form, "")
	}
	return content
}

// splitReferenced partitions the pending clipboard images into the ones
// whose placeholder still appears in text (kept) and the rest (dropped:
// the user deleted the placeholder). Kept order follows attachment
// order, so blocks join the prompt in paste order.
func splitReferenced(text string, pending []clipImage) (kept, dropped []clipImage) {
	for _, c := range pending {
		if bytes.Contains([]byte(text), []byte(c.placeholder())) {
			kept = append(kept, c)
		} else {
			dropped = append(dropped, c)
		}
	}
	return kept, dropped
}

// imageBlocks projects kept attachments onto content blocks.
func imageBlocks(kept []clipImage) []llm.Block {
	out := make([]llm.Block, 0, len(kept))
	for _, c := range kept {
		out = append(out, c.block)
	}
	return out
}

// stripPlaceholders removes attachment placeholders from text (a
// steered message must not carry literal "[图片#N]" noise).
func stripPlaceholders(text string, pending []clipImage) string {
	for _, c := range pending {
		text = bytes.NewBuffer(bytes.ReplaceAll([]byte(text), []byte(c.placeholder()), nil)).String()
	}
	return text
}

// ---------------------------------------------------------------------------
// DIB → PNG (Windows clipboard bitmaps carry no file header)
// ---------------------------------------------------------------------------

// dibToPNG converts a raw clipboard DIB to PNG. 16/24/32bpp bitmaps —
// what screenshots are — decode directly: x/image/bmp rejects
// real-world clipboard DIBs (BI_BITFIELDS with anything but the exact
// default masks, e.g. the alphaMask=0 GDI screenshots leave, and any
// mask block between header and pixels), which is why pasting
// screenshots used to fail. Paletted (≤8bpp) DIBs still wrap in a BMP
// file header and go through x/image/bmp.
func dibToPNG(dib []byte) ([]byte, error) {
	img, err := decodeDIB(dib)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("dib: re-encode: %w", err)
	}
	return out.Bytes(), nil
}

// knownDIBHeaderSize reports whether a DIB header size is one we parse.
func knownDIBHeaderSize(n uint32) bool {
	switch n {
	case 40, 52, 56, 64, 108, 124: // INFOHEADER, V3 variants, OS/2 2.x, V4, V5
		return true
	}
	return false
}

// decodeDIB parses a clipboard DIB into an image.
func decodeDIB(dib []byte) (image.Image, error) {
	if len(dib) < 40 {
		return nil, errors.New("dib: too small")
	}
	headerSize := binary.LittleEndian.Uint32(dib[0:])
	if !knownDIBHeaderSize(headerSize) || int(headerSize) > len(dib) {
		return nil, fmt.Errorf("dib: bad header size %d", headerSize)
	}
	width := int(int32(binary.LittleEndian.Uint32(dib[4:])))
	height := int(int32(binary.LittleEndian.Uint32(dib[8:])))
	planes := binary.LittleEndian.Uint16(dib[12:])
	bpp := binary.LittleEndian.Uint16(dib[14:])
	compression := binary.LittleEndian.Uint32(dib[16:])

	topDown := height < 0 // negative height = top-down row order
	if topDown {
		height = -height
	}
	if planes != 1 || width <= 0 || height <= 0 {
		return nil, fmt.Errorf("dib: bad geometry %dx%d planes=%d", width, height, planes)
	}
	switch compression { // RLE and JPEG/PNG-linked DIBs never appear on the clipboard
	case 0, 3, 6: // BI_RGB, BI_BITFIELDS, BI_ALPHABITFIELDS
	default:
		return nil, fmt.Errorf("dib: unsupported compression %d", compression)
	}

	// Channel masks and the pixel-data offset. BI_BITFIELDS(3) /
	// BI_ALPHABITFIELDS(6) carry masks inside V3+/V4/V5 headers, or in a
	// block trailing a bare 40-byte BITMAPINFOHEADER.
	var rm, gm, bm, am uint32
	pixOff := int(headerSize)
	if compression == 3 || compression == 6 {
		// Bounds check BEFORE the mask reads: the pixOff guard below ran
		// too late for a truncated payload — a short DIB would panic
		// inside le32 (the UI goroutine crashes the whole TUI).
		need := 52 // RGB masks occupy bytes [40,52)
		if headerSize >= 56 || compression == 6 {
			need = 56 // alpha-mask read (or in-header masks) touches [40,56)
		}
		if len(dib) < need {
			return nil, errors.New("dib: truncated masks")
		}
		switch {
		case headerSize >= 56: // masks (incl. alpha) inside the header
			rm, gm, bm, am = le32(dib, 40), le32(dib, 44), le32(dib, 48), le32(dib, 52)
		case headerSize == 52: // RGB inside, alpha (BI_ALPHABITFIELDS) trails
			rm, gm, bm = le32(dib, 40), le32(dib, 44), le32(dib, 48)
			if compression == 6 {
				am = le32(dib, 52)
				pixOff += 4
			}
		default: // 40-byte header: the whole mask block trails it
			rm, gm, bm = le32(dib, 40), le32(dib, 44), le32(dib, 48)
			pixOff += 12
			if compression == 6 {
				am = le32(dib, 52)
				pixOff += 4
			}
		}
		if pixOff > len(dib) {
			return nil, errors.New("dib: truncated masks")
		}
	}
	switch bpp {
	case 32:
		if rm|gm|bm == 0 {
			rm, gm, bm = 0xFF0000, 0xFF00, 0xFF // memory order BGRX
		}
	case 16:
		if rm|gm|bm == 0 {
			rm, gm, bm = 0x7C00, 0x03E0, 0x1F // BI_RGB 16bpp is RGB 5:5:5
		}
	}

	switch bpp {
	case 16, 24, 32:
		return dibPixels(dib[pixOff:], width, height, int(bpp), topDown, rm, gm, bm, am)
	case 1, 2, 4, 8:
		return palettedDIB(dib, headerSize, width, height, int(bpp), topDown)
	}
	return nil, fmt.Errorf("dib: unsupported bpp %d", bpp)
}

// dibPixels decodes 16/24/32bpp rows (each 4-byte aligned, bottom-up
// unless topDown) into an NRGBA image, applying the channel masks.
func dibPixels(pix []byte, width, height, bpp int, topDown bool, rm, gm, bm, am uint32) (image.Image, error) {
	rowBytes := (width*bpp + 31) / 32 * 4
	if len(pix) < rowBytes*height {
		return nil, fmt.Errorf("dib: truncated pixels (%d < %d)", len(pix), rowBytes*height)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		src := y
		if !topDown {
			src = height - 1 - y
		}
		row := pix[src*rowBytes:]
		dst := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for x := 0; x < width; x++ {
			d := dst[x*4:]
			switch bpp {
			case 24: // BGR triples, always opaque
				d[0], d[1], d[2], d[3] = row[x*3+2], row[x*3+1], row[x*3], 0xFF
			case 32:
				v := binary.LittleEndian.Uint32(row[x*4:])
				d[0], d[1], d[2] = mask8(v, rm), mask8(v, gm), mask8(v, bm)
				d[3] = 0xFF
				if am != 0 {
					d[3] = mask8(v, am)
				}
			case 16:
				v := uint32(binary.LittleEndian.Uint16(row[x*2:]))
				d[0], d[1], d[2] = mask8(v, rm), mask8(v, gm), mask8(v, bm)
				d[3] = 0xFF
				if am != 0 {
					d[3] = mask8(v, am)
				}
			}
		}
	}
	return img, nil
}

// mask8 extracts one channel from a masked pixel value and scales it to
// 8 bits (5- and 6-bit channels replicate their top bits into the gap).
func mask8(v, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := bits.TrailingZeros32(mask)
	n := bits.OnesCount32(mask)
	x := (v & mask) >> shift
	switch n {
	case 8:
		return uint8(x)
	case 5:
		return uint8(x<<3 | x>>2)
	case 6:
		return uint8(x<<2 | x>>4)
	default:
		return uint8(x * 255 / ((1 << n) - 1))
	}
}

// palettedDIB handles ≤8bpp DIBs by wrapping them in a BMP file header
// and letting x/image/bmp decode (compression-free palettes are all it
// accepts, and those are the only paletted clipboards in practice).
func palettedDIB(dib []byte, headerSize uint32, width, height, bpp int, _ bool) (image.Image, error) {
	colorsUsed := binary.LittleEndian.Uint32(dib[32:])
	off := 14 + int(headerSize)
	if bpp <= 8 {
		n := int(colorsUsed)
		if n == 0 {
			n = 1 << uint(bpp)
		}
		off += n * 4
	}
	if off > 14+len(dib) {
		return nil, errors.New("dib: pixel offset past end")
	}
	var buf bytes.Buffer
	buf.Grow(14 + len(dib))
	buf.WriteString("BM")
	writeLE32(&buf, uint32(14+len(dib)))
	writeLE32(&buf, 0) // reserved
	writeLE32(&buf, uint32(off))
	buf.Write(dib)
	img, err := bmp.Decode(&buf)
	if err != nil {
		return nil, fmt.Errorf("dib: decode: %w", err)
	}
	return img, nil
}

func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

func writeLE32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}
