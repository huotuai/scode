package tools

import (
	"strings"
	"testing"
)

func TestRgFormatEvent(t *testing.T) {
	match := `{"type":"match","data":{"path":{"text":"./sub/a.go"},"lines":{"text":"\tfound := true\n"},"line_number":42,"absolute_offset":100,"submatches":[]}}`
	line, isMatch := rgFormatEvent(match, ".")
	if !isMatch || line != "> sub/a.go:42: \tfound := true" {
		t.Fatalf("match event = %q, %v", line, isMatch)
	}

	ctx := `{"type":"context","data":{"path":{"text":"./sub/a.go"},"lines":{"text":"}\n"},"line_number":43,"absolute_offset":115,"submatches":[]}}`
	line, isMatch = rgFormatEvent(ctx, ".")
	if isMatch || line != "  sub/a.go-43- }" {
		t.Fatalf("context event = %q, %v", line, isMatch)
	}

	for _, skip := range []string{
		`{"type":"begin","data":{"path":{"text":"./a.go"}}}`,
		`{"type":"end","data":{"path":{"text":"./a.go"},"binary_offset":null,"stats":{}}}`,
		`{"type":"summary","data":{"stats":{}}}`,
		`not json at all`,
	} {
		if line, _ := rgFormatEvent(skip, "."); line != "" {
			t.Fatalf("non-displayable event produced output: %q", line)
		}
	}
}

func TestRgDisplayPath(t *testing.T) {
	cases := map[string]string{
		`./internal/grep.go`: "internal/grep.go",
		`.\internal\grep.go`: "internal/grep.go",
		`internal/grep.go`:   "internal/grep.go",
		`/abs/path/file.go`:  "/abs/path/file.go",
		`C:\repo\file.go`:    "C:/repo/file.go",
	}
	for in, want := range cases {
		target := "."
		if strings.HasPrefix(in, "/") || strings.Contains(in, `C:\`) {
			target = in // absolute single-file target passes through
		}
		if got := rgDisplayPath(in, target); got != want {
			t.Fatalf("rgDisplayPath(%q) = %q, want %q", in, got, want)
		}
	}
}
