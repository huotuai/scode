// Sandbox escalation: the strictly-wider ladder, argument-pairing
// validation, and the per-call judge (a port of dsh's
// dsh-sandbox/escalation.ts). The approval CHANNEL stays with the caller —
// this package only judges; tools close over the session's approver.
package sandbox

import (
	"fmt"
	"strings"
)

// WiderModes is the strictly-wider table: what a call whose effective mode
// is the key may escalate TO (checked at execution, never baked into a
// schema — schemas are registry-global, the effective mode is per-call).
var WiderModes = map[Mode][]Mode{
	ModeReadOnly:       {ModeWorkspaceWrite, ModeDangerFullAccess},
	ModeWorkspaceWrite: {ModeDangerFullAccess},
}

// EscalationTargets is the closed vocabulary advertised in tool schemas —
// every mode a call could ever escalate TO (read-only is the floor).
// Constant regardless of the deployment default: cutting it down would
// strand a session switched below the default with no lever, and a
// mode-conditional schema would break prompt-cache prefix stability.
var EscalationTargets = []Mode{ModeWorkspaceWrite, ModeDangerFullAccess}

// ValidateEscalationArgs enforces the pairing a tool schema cannot express:
// sandbox_permissions and justification travel together, and the
// justification is a non-empty sentence.
func ValidateEscalationArgs(permissions, justification string) error {
	if permissions != "" && justification == "" {
		return fmt.Errorf("invalid escalation: sandbox_permissions requires a justification")
	}
	if permissions == "" && justification != "" {
		return fmt.Errorf("invalid escalation: justification is only valid together with sandbox_permissions")
	}
	if strings.TrimSpace(justification) == "" && justification != "" {
		return fmt.Errorf("invalid justification: expected a non-empty sentence")
	}
	return nil
}

// JudgeEscalation resolves a sandbox_permissions request BEFORE anything
// executes. Repeating the effective mode needs no approval. A strictly
// wider mode needs approval and applies only to the one call. Anything
// else is an error.
func JudgeEscalation(requested string, effective Mode) (target Mode, needsApproval bool, err error) {
	mode, err := ParseMode(requested)
	if err != nil {
		return "", false, err
	}
	if mode == effective {
		return effective, false, nil
	}
	for _, w := range WiderModes[effective] {
		if w == mode {
			return mode, true, nil
		}
	}
	return "", false, fmt.Errorf("sandbox escalation to %q is not strictly wider than this call's current %q mode", mode, effective)
}

// MaxEscalationDetail bounds the detail one approval prompt shows.
const MaxEscalationDetail = 240

// detailScanLimit bounds how much raw text is even scanned: the whitespace
// collapse below splits fields, and a pathologically long payload would
// otherwise make that proportional allocation dominate the approval path.
const detailScanLimit = 4096

// SummarizeDetail renders an escalation detail (a command or a path) as one
// bounded line for the approval prompt.
//
// Two properties matter. Runs of whitespace collapse to a single space, so a
// multi-line payload cannot forge layout inside the prompt. And over-long
// text is elided in the MIDDLE, keeping head and tail: a long command's
// dangerous part is as often at the end as at the beginning, so a plain
// head-truncation would hide exactly what the human is being asked to
// approve. Both elisions are rune-safe (a command may be CJK).
func SummarizeDetail(detail string) string {
	flat := strings.Join(strings.Fields(elideMiddle(detail, detailScanLimit)), " ")
	return elideMiddle(flat, MaxEscalationDetail)
}

// elideMiddle keeps the head and the tail of s, capped at max runes.
func elideMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	return string(r[:head]) + fmt.Sprintf(" … (%d more chars) … ", len(r)-max) + string(r[len(r)-tail:])
}

// DenialSignatures maps each runner dialect to the text its kernel refusal
// surfaces in command output (dsh's per-runner denialSignatures table).
//
// The dialect scoping matters: "permission denied" is a Landlock refusal on
// Linux but an ordinary tool failure on Windows, and "read-only file system"
// can only ever come from bwrap's ro-bind. A flat list spanning platforms
// would mislabel ordinary failures as sandbox denials, which pushes the model
// into an unnecessary escalation.
var DenialSignatures = map[string][]string{
	"windows-acl": {"access is denied", "access to the path", "permission denied", "operation not permitted", "拒绝访问"},
	"bwrap":       {"read-only file system"},
	"landlock":    {"permission denied"},
	"seatbelt":    {"operation not permitted"},
}

// OutputLooksDenied reports whether confined-command output carries the
// ACTIVE runner's denial signature (only meaningful for a non-zero exit under
// confinement). It stays a heuristic — a command denied by ordinary
// permissions can read the same — but it never fires on another platform's
// dialect, which is what the per-dialect table buys.
func OutputLooksDenied(output string) bool {
	lower := strings.ToLower(output)
	for _, sig := range activeDenialSignatures() {
		if strings.Contains(lower, strings.ToLower(sig)) {
			return true
		}
	}
	return false
}
