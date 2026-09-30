//go:build windows

// Windows ACL confinement backend — a Go port of dsh's
// @deepseek-ai/dsh-sandbox-windows-acl (MIT, (c) 2026 DeepSeek).
//
// Mechanism: the child runs under a WRITE_RESTRICTED restricted token
// whose restricting-SID list carries the keep-alive group (logon SID +
// Everyone) plus, under workspace-write, two capability SIDs — a
// deterministic per-workspace SID and a per-run private-temp SID. The
// granted roots carry the capability allow ACE, a world-SID deny on the
// ambient FILE_DELETE_CHILD right, and a Low no-write-up mandatory label;
// the token is lowered to Low integrity so the labels bite. Windows runs
// two access checks (normal SIDs, then restricting SIDs) and grants
// write-class access only when both pass.
//
// Fail-closed everywhere: any Win32 failure aborts before the spawn —
// the child NEVER runs unrestricted.
package sandbox

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 constants (dsh win32-abi.ts, trimmed to what this port touches).
const (
	tokenDuplicate       = 0x0002
	tokenAssignPrimary   = 0x0001
	tokenQuery           = 0x0008
	tokenAdjustDefault   = 0x0080
	seGroupLogonID       = 0xC0000000
	seGroupIntegrity     = 0x00000020
	fileGenericWrite     = 0x00120116
	deleteRight          = 0x00010000
	fileDeleteChild      = 0x0040
	standardRightsWrite  = 0x00020000
	fileAllAccess        = 0x1F01FF
	grantMask            = (fileGenericWrite | deleteRight | fileDeleteChild) &^ standardRightsWrite
	disableMaxPrivilege  = 0x1
	luaToken             = 0x4
	writeRestricted      = 0x8
	tokenIntegrityLevel  = 25
	tokenDefaultDacl     = 6
	daclSecurityInfo     = 0x00000004
	labelSecurityInfo    = 0x00000010
	mandatoryLabelAce    = 0x11
	mandatoryNoWriteUp   = 0x00000001
	aclRevision          = 2
	seFileObject         = 1
	grantAccess          = 1
	denyAccess           = 3
	revokeAccess         = 4
	inheritAll           = 0x3 // SUB_CONTAINERS_AND_OBJECTS_INHERIT
	inheritContainers    = 0x2 // CONTAINER_INHERIT_ACE
	accessAllowedAceType = 0
	accessDeniedAceType  = 1
	lockfileExclusive    = 0x2
)

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
	procSetEntriesInAclW      = advapi32.NewProc("SetEntriesInAclW")
	procInitializeAcl         = advapi32.NewProc("InitializeAcl")
	procAddMandatoryAce       = advapi32.NewProc("AddMandatoryAce")
)

// aclError carries the API name and the exact Win32 code (dsh's Win32Error
// discipline — every call checked, nothing silent).
type aclError struct {
	api    string
	code   uintptr
	detail string
}

func (e *aclError) Error() string {
	if e.code != 0 {
		return fmt.Sprintf("windows-acl: %s failed (win32 %d): %s", e.api, e.code, e.detail)
	}
	return fmt.Sprintf("windows-acl: %s failed: %s", e.api, e.detail)
}

func aclErr(api string, code uintptr, detail string) error {
	return &aclError{api: api, code: code, detail: detail}
}

// errnoToCode extracts the Win32 error code from a syscall errno.
func errnoToCode(errno error) uintptr {
	if e, ok := errno.(syscall.Errno); ok {
		return uintptr(e)
	}
	return 0
}

// ---------------------------------------------------------------------------
// capability SID derivation (dsh workspace-sid.ts, verbatim)
// ---------------------------------------------------------------------------

func deriveSid(seedParts ...string) string {
	h := sha256.New()
	for _, p := range seedParts {
		h.Write([]byte(p))
	}
	d := h.Sum(nil)
	first := binary.LittleEndian.Uint32(d[0:4])%(1<<30-1) + 1
	second := binary.LittleEndian.Uint32(d[4:8])%(1<<30-1) + 1
	return fmt.Sprintf("S-1-4-%d-%d", first, second)
}

// workspaceWriteSid is the deterministic per-workspace capability identity
// (canonical path in → same SID across sessions/restarts; standing ACE reuse).
func workspaceWriteSid(workspaceRoot string) string { return deriveSid(workspaceRoot) }

// tempWriteSid is one private temp directory's capability identity; the "1"
// domain-separates it from two-subauthority workspace SIDs.
func tempWriteSid(tempDir string) string { return deriveSid("temp\x00", tempDir) + "-1" }

// parseSid converts an SDDL string to a SID allocation (LocalAlloc'd by
// Windows; freed with localFreeSid).
func parseSid(sddl string) (*windows.SID, error) {
	var sid *windows.SID
	if err := windows.ConvertStringSidToSid(syscall.StringToUTF16Ptr(sddl), &sid); err != nil {
		return nil, aclErr("ConvertStringSidToSidW", 0, sddl)
	}
	return sid, nil
}

func localFreeSid(sid *windows.SID) {
	if sid != nil {
		windows.LocalFree(windows.Handle(unsafe.Pointer(sid))) //nolint:errcheck // best-effort cleanup
	}
}

// ---------------------------------------------------------------------------
// per-path lock (dsh withPathLock): serializes read-merge-write DACL edits
// ---------------------------------------------------------------------------

func lockFilePath(path string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(path)))
	name := fmt.Sprintf("%x", digest[:])[:16] + ".lock"
	return filepath.Join(os.TempDir(), "scode-acl-locks", name)
}

func withPathLock(path string, action func() error) error {
	lp := lockFilePath(path)
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return aclErr("MkdirAll", 0, lp)
	}
	lpUTF16, err := syscall.UTF16PtrFromString(lp)
	if err != nil {
		return err
	}
	// No FILE_SHARE_DELETE: a deletable lock file could be replaced under
	// the holder, letting two processes hold "the same" lock.
	h, err := windows.CreateFile(lpUTF16, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, 0, 0)
	if err != nil {
		return aclErr("CreateFileW", 0, lp)
	}
	var ov windows.Overlapped // zeroed: offset 0, hEvent NULL (synchronous handle)
	if err := windows.LockFileEx(h, lockfileExclusive, 0, 1, 0, &ov); err != nil {
		windows.CloseHandle(h) //nolint:errcheck
		return aclErr("LockFileEx", 0, lp)
	}
	actionErr := action()
	if err := windows.UnlockFileEx(h, 0, 1, 0, &ov); err != nil && actionErr == nil {
		actionErr = aclErr("UnlockFileEx", 0, lp)
	}
	if err := windows.CloseHandle(h); err != nil && actionErr == nil {
		actionErr = aclErr("CloseHandle", 0, lp)
	}
	return actionErr
}

// ---------------------------------------------------------------------------
// ACL reading and walking
// ---------------------------------------------------------------------------

// aclBytes views an ACL's raw memory (the pointer lives INSIDE the owning
// security-descriptor allocation — see readCurrentSecurity's contract).
func aclBytes(acl *windows.ACL) []byte {
	size := int(uint16At(unsafe.Pointer(acl), 2))
	if size < 8 || size > 1<<20 {
		return nil // implausible: callers fall back to the merge path
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(acl)), size)
}

func uint8At(p unsafe.Pointer, off int) uint8   { return *(*uint8)(unsafe.Add(p, off)) }
func uint16At(p unsafe.Pointer, off int) uint16 { return *(*uint16)(unsafe.Add(p, off)) }
func uint32At(p unsafe.Pointer, off int) uint32 { return *(*uint32)(unsafe.Add(p, off)) }

// hasExactEntry reports whether the ACL carries the EXACT entry
// (aceType, inheritance, mask, trustee SID). The ACE's SID is INLINE at
// offset 8 — compared field-by-field via EqualSid (dsh sameSidAt).
func hasExactEntry(acl *windows.ACL, aceType uint8, inheritance uint8, mask uint32, sid *windows.SID) bool {
	b := aclBytes(acl)
	if b == nil {
		return false
	}
	aceCount := int(uint16At(unsafe.Pointer(acl), 4))
	offset := 8 // first ACE follows the 8-byte ACL header
	for i := 0; i < aceCount; i++ {
		if offset+8 > len(b) {
			return false
		}
		aceSize := int(binary.LittleEndian.Uint16(b[offset+2:]))
		if aceSize < 8 || offset+aceSize > len(b) {
			return false
		}
		exact := b[offset] == aceType && b[offset+1] == inheritance &&
			binary.LittleEndian.Uint32(b[offset+4:]) == mask
		if exact {
			inlineSid := (*windows.SID)(unsafe.Pointer(&b[offset+8]))
			if windows.EqualSid(inlineSid, sid) {
				return true
			}
		}
		offset += aceSize
	}
	return false
}

// hasForeignGrant reports whether a capability grant for a SID OTHER than
// sid stands on this DACL (the revoke keeps the shared Low label then).
func hasForeignGrant(acl *windows.ACL, sid *windows.SID) bool {
	b := aclBytes(acl)
	if b == nil {
		return false
	}
	aceCount := int(uint16At(unsafe.Pointer(acl), 4))
	offset := 8
	for i := 0; i < aceCount; i++ {
		if offset+8 > len(b) {
			return false
		}
		aceSize := int(binary.LittleEndian.Uint16(b[offset+2:]))
		if aceSize < 8 || offset+aceSize > len(b) {
			return false
		}
		isGrant := b[offset] == accessAllowedAceType && binary.LittleEndian.Uint32(b[offset+4:]) == grantMask
		if isGrant {
			inlineSid := (*windows.SID)(unsafe.Pointer(&b[offset+8]))
			if !windows.EqualSid(inlineSid, sid) {
				return true
			}
		}
		offset += aceSize
	}
	return false
}

// ---------------------------------------------------------------------------
// grant / revoke (dsh acl.ts grantWrite / revokeWrite)
// ---------------------------------------------------------------------------

// setEntriesInAclW wraps advapi32.SetEntriesInAclW (unexported in x/sys).
// The result ACL is LocalAlloc'd by Windows — caller LocalFrees it.
// The out-slot is heap-allocated: pointers into small stack frames have
// empirically tripped the kernel's user-buffer probe (ERROR_NOACCESS).
func setEntriesInAclW(entries []windows.EXPLICIT_ACCESS, oldAcl *windows.ACL) (*windows.ACL, error) {
	slot := new(*windows.ACL)
	r0, _, _ := procSetEntriesInAclW.Call(
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(&entries[0])),
		uintptr(unsafe.Pointer(oldAcl)),
		uintptr(unsafe.Pointer(slot)),
	)
	if r0 != 0 {
		return nil, aclErr("SetEntriesInAclW", r0, "merge")
	}
	if *slot == nil {
		return nil, aclErr("SetEntriesInAclW", 0, "null merged ACL")
	}
	return *slot, nil
}

func buildExplicitAccess(sid *windows.SID, mode uint32, perms uint32, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(perms),
		AccessMode:        windows.ACCESS_MODE(mode),
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
			TrusteeForm:              windows.TRUSTEE_IS_SID,
			TrusteeType:              windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue:             windows.TrusteeValueFromSID(sid),
		},
	}
}

// buildLowLabelAcl builds the Low mandatory label ACL (one inheritable
// SYSTEM_MANDATORY_LABEL_ACE, no-write-up). Go memory suffices: Windows
// only reads/copies the ACL synchronously during SetNamedSecurityInfoW.
func buildLowLabelAcl(lowSid *windows.SID) ([]byte, error) {
	sidLen := windows.GetLengthSid(lowSid)
	aclLen := 8 + 8 + int(sidLen) // header + mandatory-ACE overhead + SID
	buf := make([]byte, aclLen)
	acl := unsafe.Pointer(&buf[0])
	r1, _, errno := procInitializeAcl.Call(uintptr(acl), uintptr(aclLen), uintptr(aclRevision))
	if r1 == 0 {
		return nil, aclErr("InitializeAcl", errnoToCode(errno), "Low mandatory label ACL")
	}
	r1, _, errno = procAddMandatoryAce.Call(
		uintptr(acl), uintptr(aclRevision), uintptr(inheritAll),
		uintptr(mandatoryNoWriteUp), uintptr(unsafe.Pointer(lowSid)),
	)
	if r1 == 0 {
		return nil, aclErr("AddMandatoryAce", errnoToCode(errno), "Low mandatory label ACL")
	}
	return buf, nil
}

// readCurrentSecurity reads the directory's explicit DACL and mandatory
// label. x/sys's GetNamedSecurityInfo deep-copies the self-relative
// descriptor into GO memory (and LocalFrees the win-heap block itself), so
// the returned ACLs are valid while sd is alive — keep it referenced with
// runtime.KeepAlive; there is NOTHING to free.
func readCurrentSecurity(path string) (dacl *windows.ACL, label *windows.ACL, sd *windows.SECURITY_DESCRIPTOR, err error) {
	sd, err = windows.GetNamedSecurityInfo(path, seFileObject, daclSecurityInfo|labelSecurityInfo)
	if err != nil {
		return nil, nil, nil, aclErr("GetNamedSecurityInfoW", 0, path)
	}
	dacl, _, _ = sd.DACL()
	label, _, _ = sd.SACL()
	return dacl, label, sd, nil
}

// mergeAndApply merges entries into oldAcl, applies the merged DACL plus
// the label edit in one SetNamedSecurityInfoW, then frees the merged ACL
// (SetNamedSecurityInfoW copies). oldAcl/label live in Go memory (see
// readCurrentSecurity); sd is kept alive until the merge has consumed them.
func mergeAndApply(path string, entries []windows.EXPLICIT_ACCESS, oldAcl *windows.ACL, label []byte, clearLabel bool, sd *windows.SECURITY_DESCRIPTOR) error {
	newAcl, err := setEntriesInAclW(entries, oldAcl)
	runtime.KeepAlive(sd)
	if err != nil {
		return err
	}

	securityInfo := uint32(daclSecurityInfo)
	var sacl *windows.ACL
	if label != nil {
		securityInfo |= labelSecurityInfo
		sacl = (*windows.ACL)(unsafe.Pointer(&label[0]))
	} else if clearLabel {
		securityInfo |= labelSecurityInfo
		sacl = nil // clearing: set an empty SACL
	}
	applyErr := windows.SetNamedSecurityInfo(path, seFileObject, windows.SECURITY_INFORMATION(securityInfo), nil, nil, newAcl, sacl)
	windows.LocalFree(windows.Handle(unsafe.Pointer(newAcl))) //nolint:errcheck
	if applyErr != nil {
		return aclErr("SetNamedSecurityInfoW", errnoToCode(applyErr), path)
	}
	return nil
}

// aclErrCode extracts the win32 code from an aclError (0 = none/other).
func aclErrCode(err error) uintptr {
	var e *aclError
	if errors.As(err, &e) {
		return e.code
	}
	return 0
}

const (
	errorAccessDenied = 5
	writeOwner        = 0x00080000
)

// grantWrite grants grantMask (Write+Delete, displays "Modify") to the
// capability SID, denies the world SID the ambient FILE_DELETE_CHILD right
// (containers only — the bit is meaningless on files and would break
// FullControl opens), and applies the Low no-write-up label. Idempotent:
// the exact ACE+deny+label standing skips the eager full-tree propagation.
func grantWrite(path string, sid, lowSid, worldSid *windows.SID) error {
	return withPathLock(path, func() error {
		err := grantWriteOnce(path, sid, lowSid, worldSid)
		if aclErrCode(err) == errorAccessDenied {
			// The mandatory label lives in the SACL; applying it needs
			// WRITE_OWNER on the directory, and an owner-implicit DACL (e.g.
			// secondary-drive roots granting Authenticated Users merely
			// Modify) lacks it. The owner implicitly holds WRITE_DAC, which
			// legitimately grants ourselves WRITE_OWNER — then retry ONCE
			// (dsh fails loud here; scode self-repairs the ownership right,
			// a no-op privilege-wise for a directory the user already owns).
			if e := ensureWriteOwner(path); e == nil {
				err = grantWriteOnce(path, sid, lowSid, worldSid)
			}
		}
		return err
	})
}

// ensureWriteOwner grants the calling user WRITE_OWNER on the directory
// (this object only) so the mandatory-label write succeeds. The owner can
// always WRITE_DAC its own directory, so this adds no new privilege.
func ensureWriteOwner(path string) error {
	token, err := openCurrentProcessToken()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(windows.Handle(token)) //nolint:errcheck
	user, err := token.GetTokenUser()
	if err != nil {
		return aclErr("GetTokenInformation", 0, "TokenUser")
	}
	oldAcl, _, sd, err := readCurrentSecurity(path)
	if err != nil {
		return err
	}
	if oldAcl != nil && hasExactEntry(oldAcl, accessAllowedAceType, 0, writeOwner, user.User.Sid) {
		return nil
	}
	return mergeAndApply(path, []windows.EXPLICIT_ACCESS{
		buildExplicitAccess(user.User.Sid, grantAccess, writeOwner, 0),
	}, oldAcl, nil, false, sd)
}

// grantWriteOnce is the unlocked body of grantWrite — callers MUST hold
// the path lock (taking it here would self-deadlock: LockFileEx on the
// same lock file does not nest).
func grantWriteOnce(path string, sid, lowSid, worldSid *windows.SID) error {
	oldAcl, labelAcl, sd, err := readCurrentSecurity(path)
	if err != nil {
		return err
	}
	if oldAcl != nil && labelAcl != nil &&
		hasExactEntry(oldAcl, accessAllowedAceType, inheritAll, grantMask, sid) &&
		hasExactEntry(oldAcl, accessDeniedAceType, inheritContainers, fileDeleteChild, worldSid) &&
		hasExactEntry(labelAcl, mandatoryLabelAce, inheritAll, mandatoryNoWriteUp, lowSid) {
		return nil // exact ACE+deny+label stand: nothing to do
	}
	label, err := buildLowLabelAcl(lowSid)
	if err != nil {
		return err
	}
	return mergeAndApply(path, []windows.EXPLICIT_ACCESS{
		buildExplicitAccess(worldSid, denyAccess, fileDeleteChild, inheritContainers),
		buildExplicitAccess(sid, grantAccess, grantMask, inheritAll),
	}, oldAcl, label, false, sd)
}

// revokeWrite removes every ACE for the capability SID (other entries
// survive). The shared Low label is cleared only when no other capability
// grant remains on the directory.
func revokeWrite(path string, sid *windows.SID) error {
	return withPathLock(path, func() error {
		oldAcl, _, sd, err := readCurrentSecurity(path)
		if err != nil {
			return err
		}
		if oldAcl == nil {
			return nil
		}
		keepLabel := hasForeignGrant(oldAcl, sid)
		return mergeAndApply(path, []windows.EXPLICIT_ACCESS{
			buildExplicitAccess(sid, revokeAccess, 0, inheritAll),
		}, oldAcl, nil, !keepLabel, sd)
	})
}

// ---------------------------------------------------------------------------
// restricted token (dsh token.ts)
// ---------------------------------------------------------------------------

// openCurrentProcessToken opens the process token with the rights
// CreateRestrictedToken and the label/default-DACL edits require.
func openCurrentProcessToken() (windows.Token, error) {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(),
		tokenQuery|tokenDuplicate|tokenAdjustDefault|tokenAssignPrimary, &token)
	if err != nil {
		return 0, aclErr("OpenProcessToken", 0, "current process")
	}
	return token, nil
}

// findLogonSid copies the token's logon session SID (S-1-5-5-x-y,
// SE_GROUP_LOGON_ID). The restricted token needs it for WinSta0/desktop and
// CNG (without it: 0xC0000142 / 0xE0434352 crashes — dsh's keep-alive group).
func findLogonSid(token windows.Token) (*windows.SID, []byte, error) {
	groups, err := token.GetTokenGroups()
	if err != nil {
		return nil, nil, aclErr("GetTokenInformation", 0, "TokenGroups")
	}
	for _, g := range groups.AllGroups() {
		if g.Attributes&seGroupLogonID != seGroupLogonID {
			continue
		}
		sidLen := windows.GetLengthSid(g.Sid)
		buf := make([]byte, sidLen)
		copySid := (*windows.SID)(unsafe.Pointer(&buf[0]))
		if err := windows.CopySid(sidLen, copySid, g.Sid); err != nil {
			return nil, nil, aclErr("CopySid", 0, "logon SID")
		}
		return copySid, buf, nil
	}
	return nil, nil, aclErr("GetTokenInformation", 0, "no logon SID among token groups")
}

// createRestrictedToken builds the WRITE_RESTRICTED token with the
// mode-selected restricting list: [logon, Everyone] under read-only;
// [logon, Everyone, workspaceSid, tempSid?] under workspace-write.
func createRestrictedToken(base windows.Token, logonSid, worldSid *windows.SID, writeSids []*windows.SID, mode Mode) (windows.Token, error) {
	sids := []windows.SIDAndAttributes{{Sid: logonSid}, {Sid: worldSid}}
	if mode == ModeWorkspaceWrite {
		if len(writeSids) == 0 {
			return 0, aclErr("CreateRestrictedToken", 0, "workspace-write requires at least one write SID")
		}
		for _, s := range writeSids {
			sids = append(sids, windows.SIDAndAttributes{Sid: s})
		}
	}
	slot := new(windows.Token) // heap out-slot, see setEntriesInAclW
	r1, _, errno := procCreateRestrictedToken.Call(
		uintptr(base),
		uintptr(disableMaxPrivilege|luaToken|writeRestricted),
		0, 0, // no SIDs disabled
		0, 0, // no privileges deleted
		uintptr(len(sids)),
		uintptr(unsafe.Pointer(&sids[0])),
		uintptr(unsafe.Pointer(slot)),
	)
	if r1 == 0 {
		return 0, aclErr("CreateRestrictedToken", errnoToCode(errno), fmt.Sprintf("%d restricting SIDs", len(sids)))
	}
	return *slot, nil
}

// restrictTokenIntegrity lowers the token to Low integrity (S-1-16-4096) —
// a token left at Medium would ignore the mandatory labels.
func restrictTokenIntegrity(token windows.Token, lowSid *windows.SID) error {
	label := make([]byte, 16) // TOKEN_MANDATORY_LABEL { SID_AND_ATTRIBUTES Label }; heap, see above
	binary.LittleEndian.PutUint64(label[0:8], uint64(uintptr(unsafe.Pointer(lowSid))))
	binary.LittleEndian.PutUint32(label[8:12], seGroupIntegrity)
	if err := windows.SetTokenInformation(token, tokenIntegrityLevel, &label[0], uint32(len(label))); err != nil {
		return aclErr("SetTokenInformation", errnoToCode(err), "TokenIntegrityLevel (Low)")
	}
	return nil
}

// setTokenDefaultDaclGrant merges one full-access allow ACE for a
// RESTRICTING SID into the token's DEFAULT DACL. Without it every new
// object the confined process creates (anonymous stdio pipes!) fails the
// write pass-2 check and piped-stdio grandchild spawns break (dsh's EPERM
// fix). Object creation stays gated by the parent container's DACL.
func setTokenDefaultDaclGrant(token windows.Token, sid *windows.SID) error {
	var needed uint32
	windows.GetTokenInformation(token, tokenDefaultDacl, nil, 0, &needed) //nolint:errcheck // expected ERROR_INSUFFICIENT_BUFFER
	if needed == 0 {
		return aclErr("GetTokenInformation", 0, "TokenDefaultDacl size query")
	}
	buf := make([]byte, needed)
	if err := windows.GetTokenInformation(token, tokenDefaultDacl, &buf[0], needed, &needed); err != nil {
		return aclErr("GetTokenInformation", 0, "TokenDefaultDacl")
	}
	currentDacl := *(**windows.ACL)(unsafe.Pointer(&buf[0]))
	if currentDacl == nil {
		return aclErr("GetTokenInformation", 0, "token carries no default DACL")
	}
	newAcl, err := setEntriesInAclW([]windows.EXPLICIT_ACCESS{
		buildExplicitAccess(sid, grantAccess, fileAllAccess, inheritAll),
	}, currentDacl)
	if err != nil {
		return err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(newAcl))) //nolint:errcheck
	// Heap-allocated info block: a stack-resident TOKEN_DEFAULT_DACL
	// intermittently fails the kernel's buffer probe (ERROR_NOACCESS).
	info := make([]byte, 8) // TOKEN_DEFAULT_DACL { PACL }
	binary.LittleEndian.PutUint64(info, uint64(uintptr(unsafe.Pointer(newAcl))))
	if err := windows.SetTokenInformation(token, tokenDefaultDacl, &info[0], uint32(len(info))); err != nil {
		return aclErr("SetTokenInformation", errnoToCode(err), "TokenDefaultDacl")
	}
	return nil
}
