package llm

import "bytes"

// imageTypeSniffBytes bounds how far into a payload DetectImageMIME looks;
// provider gateways only inspect the leading header (pi's
// IMAGE_TYPE_SNIFF_BYTES).
const imageTypeSniffBytes = 4100

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// DetectImageMIME sniffs the leading bytes for the raster image signatures
// providers accept and returns the media type, or "" when the payload is
// empty or unrecognized (pi's detectSupportedImageMimeType).
//
// Detection is by content, never by file extension. A tool that read an
// empty or mislabeled file must not become an image block: tool results
// ride the transcript into every later provider request, and a gateway
// that cannot decode the image rejects the whole request — so one bad
// block poisons the session until it is manually removed (Kimi reports an
// empty payload as "unsupported image format: text/plain; charset=utf-8").
func DetectImageMIME(data []byte) string {
	buf := data
	if len(buf) > imageTypeSniffBytes {
		buf = buf[:imageTypeSniffBytes]
	}
	switch {
	case len(buf) >= 3 && buf[0] == 0xff && buf[1] == 0xd8 && buf[2] == 0xff:
		if len(buf) >= 4 && buf[3] == 0xf7 { // JPEG-LS: not a supported JPEG
			return ""
		}
		return "image/jpeg"
	case len(buf) >= 8 && bytes.Equal(buf[:8], pngSignature):
		if !isValidPNG(buf) || isAnimatedPNG(buf) {
			return ""
		}
		return "image/png"
	case len(buf) >= 6 && (string(buf[:6]) == "GIF87a" || string(buf[:6]) == "GIF89a"):
		return "image/gif"
	case len(buf) >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP":
		return "image/webp"
	case len(buf) >= 2 && buf[0] == 'B' && buf[1] == 'M':
		if !isValidBMP(buf) {
			return ""
		}
		return "image/bmp"
	}
	return ""
}

// isValidPNG requires an IHDR chunk of the expected length so a truncated
// or mislabeled payload is not mistaken for a PNG (pi's isPng).
func isValidPNG(buf []byte) bool {
	return len(buf) >= 16 && readUint32BE(buf, len(pngSignature)) == 13 && string(buf[12:16]) == "IHDR"
}

// isAnimatedPNG reports whether an acTL chunk precedes IDAT; APNG is not a
// provider-safe still image (pi's isAnimatedPng).
func isAnimatedPNG(buf []byte) bool {
	offset := len(pngSignature)
	for offset+8 <= len(buf) {
		chunkLength := readUint32BE(buf, offset)
		chunkType := offset + 4
		if string(buf[chunkType:chunkType+4]) == "acTL" {
			return true
		}
		if string(buf[chunkType:chunkType+4]) == "IDAT" {
			return false
		}
		nextOffset := offset + 8 + int(chunkLength) + 4
		if nextOffset <= offset || nextOffset > len(buf) {
			return false
		}
		offset = nextOffset
	}
	return false
}

// isValidBMP applies pi's isBmp header sanity checks (declared size,
// pixel-data offset, DIB header, planes, bit depth).
func isValidBMP(buf []byte) bool {
	if len(buf) < 26 {
		return false
	}
	declaredFileSize := readUint32LE(buf, 2)
	pixelDataOffset := readUint32LE(buf, 10)
	dibHeaderSize := readUint32LE(buf, 14)
	if declaredFileSize != 0 && declaredFileSize < 26 {
		return false
	}
	if pixelDataOffset < 14+dibHeaderSize {
		return false
	}
	if declaredFileSize != 0 && pixelDataOffset >= declaredFileSize {
		return false
	}

	var colorPlanes, bitsPerPixel uint32
	switch {
	case dibHeaderSize == 12:
		colorPlanes = readUint16LE(buf, 22)
		bitsPerPixel = readUint16LE(buf, 24)
	case dibHeaderSize >= 40 && dibHeaderSize <= 124:
		if len(buf) < 30 {
			return false
		}
		colorPlanes = readUint16LE(buf, 26)
		bitsPerPixel = readUint16LE(buf, 28)
	default:
		return false
	}
	if colorPlanes != 1 {
		return false
	}
	switch bitsPerPixel {
	case 1, 4, 8, 16, 24, 32:
		return true
	}
	return false
}

func readUint16LE(buf []byte, offset int) uint32 {
	return uint32(buf[offset]) + uint32(buf[offset+1])<<8
}

func readUint32BE(buf []byte, offset int) uint32 {
	return uint32(buf[offset])<<24 | uint32(buf[offset+1])<<16 | uint32(buf[offset+2])<<8 | uint32(buf[offset+3])
}

func readUint32LE(buf []byte, offset int) uint32 {
	return uint32(buf[offset]) | uint32(buf[offset+1])<<8 | uint32(buf[offset+2])<<16 | uint32(buf[offset+3])<<24
}
