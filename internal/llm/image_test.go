package llm

import "testing"

func pngHeader() []byte {
	buf := append([]byte{}, pngSignature...)
	buf = append(buf, 0, 0, 0, 13) // IHDR chunk length
	buf = append(buf, 'I', 'H', 'D', 'R')
	return buf
}

func bmpHeader() []byte {
	buf := make([]byte, 30)
	buf[0], buf[1] = 'B', 'M'
	putUint32LE(buf, 2, 30)  // declared file size
	putUint32LE(buf, 10, 26) // pixel-data offset
	putUint32LE(buf, 14, 12) // BITMAPCOREHEADER
	putUint16LE(buf, 22, 1)  // color planes
	putUint16LE(buf, 24, 24) // bits per pixel
	return buf
}

func putUint16LE(buf []byte, offset int, v uint32) {
	buf[offset] = byte(v)
	buf[offset+1] = byte(v >> 8)
}

func putUint32LE(buf []byte, offset int, v uint32) {
	buf[offset] = byte(v)
	buf[offset+1] = byte(v >> 8)
	buf[offset+2] = byte(v >> 16)
	buf[offset+3] = byte(v >> 24)
}

func TestDetectImageMIME(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"nil", nil, ""},
		{"empty", []byte{}, ""},
		{"text", []byte("hello, not an image"), ""},
		{"png", pngHeader(), "image/png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}, "image/jpeg"},
		{"jpeg-ls", []byte{0xff, 0xd8, 0xff, 0xf7, 0x00}, ""},
		{"gif", []byte("GIF89a....."), "image/gif"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), 0x00), "image/webp"},
		{"bmp", bmpHeader(), "image/bmp"},
		{"byte-NUL", []byte{0x00}, ""},
	}
	for _, tc := range cases {
		if got := DetectImageMIME(tc.data); got != tc.want {
			t.Errorf("%s: DetectImageMIME = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A PNG signature without a valid IHDR chunk is a truncated/mislabeled
// payload, not an image (pi's isPng).
func TestDetectImageMIMErejectsTruncatedPNG(t *testing.T) {
	if got := DetectImageMIME(pngSignature); got != "" {
		t.Fatalf("truncated PNG detected as %q", got)
	}
}
