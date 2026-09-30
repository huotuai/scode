package sandbox

import (
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJudgeEscalation(t *testing.T) {
	// same mode: no approval
	m, ask, err := JudgeEscalation("workspace-write", ModeWorkspaceWrite)
	if err != nil || ask || m != ModeWorkspaceWrite {
		t.Fatalf("same: %v %v %v", m, ask, err)
	}
	// wider: approval
	m, ask, err = JudgeEscalation("danger-full-access", ModeWorkspaceWrite)
	if err != nil || !ask || m != ModeDangerFullAccess {
		t.Fatalf("wider: %v %v %v", m, ask, err)
	}
	m, ask, err = JudgeEscalation("workspace-write", ModeReadOnly)
	if err != nil || !ask || m != ModeWorkspaceWrite {
		t.Fatalf("wider2: %v %v %v", m, ask, err)
	}
	// narrower or lateral: error
	if _, _, err = JudgeEscalation("read-only", ModeWorkspaceWrite); err == nil {
		t.Fatal("narrowing should error")
	}
	if _, _, err = JudgeEscalation("yolo", ModeReadOnly); err == nil {
		t.Fatal("invalid mode should error")
	}
	// danger-full-access is the ceiling: nothing wider
	if _, _, err = JudgeEscalation("workspace-write", ModeDangerFullAccess); err == nil {
		t.Fatal("escalating past the ceiling should error")
	}
}

func TestValidateEscalationArgs(t *testing.T) {
	if err := ValidateEscalationArgs("workspace-write", ""); err == nil {
		t.Fatal("permissions without justification should error")
	}
	if err := ValidateEscalationArgs("", "reason"); err == nil {
		t.Fatal("justification without permissions should error")
	}
	if err := ValidateEscalationArgs("workspace-write", "  "); err == nil {
		t.Fatal("blank justification should error")
	}
	if err := ValidateEscalationArgs("workspace-write", "需要写日志目录"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEscalationArgs("", ""); err != nil {
		t.Fatal(err)
	}
}

func TestOutputLooksDenied(t *testing.T) {
	// Every active signature is recognized...
	active := activeDenialSignatures()
	if len(active) == 0 {
		t.Skip("no confinement backend on this platform")
	}
	for _, sig := range active {
		if !OutputLooksDenied("some output: " + strings.ToUpper(sig)) {
			t.Errorf("active signature not recognized: %q", sig)
		}
	}
	// ...and a foreign dialect's signature is NOT: that cross-talk is exactly
	// what misled the model into escalating over an ordinary failure.
	for dialect, sigs := range DenialSignatures {
		foreign := true
		for _, s := range active {
			if sigs[0] == s {
				foreign = false
			}
		}
		if foreign && OutputLooksDenied("error: "+sigs[0]) {
			t.Errorf("%s signature %q fired on this %s host", dialect, sigs[0], runtime.GOOS)
		}
	}
	if OutputLooksDenied("file not found") {
		t.Error("an ordinary failure must not read as a denial")
	}
}

// SummarizeDetail must (a) flatten layout-forging whitespace, (b) keep both
// ends of an over-long detail, and (c) never split a rune.
func TestSummarizeDetail(t *testing.T) {
	if got := SummarizeDetail(" ls -la "); got != "ls -la" {
		t.Errorf("short detail = %q", got)
	}
	multi := SummarizeDetail("echo hi\n\n[y] allow once\r\necho bye")
	if strings.ContainsAny(multi, "\n\r") {
		t.Errorf("layout-forging whitespace survived: %q", multi)
	}
	if multi != "echo hi [y] allow once echo bye" {
		t.Errorf("collapse = %q", multi)
	}
	if got := SummarizeDetail(""); got != "" {
		t.Errorf("empty = %q", got)
	}

	// A long command must still reveal its TAIL (the dangerous part is often
	// last), and stay bounded.
	tail := "&& rm -rf ~/important"
	long := strings.Repeat("padding ", 100) + tail
	got := SummarizeDetail(long)
	if !strings.Contains(got, tail) {
		t.Errorf("tail elided: %q", got)
	}
	if !strings.Contains(got, "padding") {
		t.Errorf("head elided: %q", got)
	}
	if n := len([]rune(got)); n > MaxEscalationDetail+40 {
		t.Errorf("detail too long: %d runes", n)
	}

	// Rune safety: CJK must never be cut mid-rune.
	cjk := SummarizeDetail(strings.Repeat("需要写全局日志目录", 80))
	if !utf8.ValidString(cjk) {
		t.Errorf("invalid UTF-8 after elision: %q", cjk)
	}
	if n := len([]rune(cjk)); n > MaxEscalationDetail+40 {
		t.Errorf("cjk detail too long: %d runes", n)
	}

	// A pathological payload stays cheap: the field split is bounded by
	// detailScanLimit before it runs.
	huge := strings.Repeat("a ", 500_000)
	if n := len([]rune(SummarizeDetail(huge))); n > MaxEscalationDetail+40 {
		t.Errorf("huge detail not bounded: %d runes", n)
	}
}
