//go:build windows

package sshauth

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	pNetApiBufferFree = netapi32.NewProc("NetApiBufferFree")
	pLookupAccountSID = advapi32.NewProc("LookupAccountSidW")
	pRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	pRegCloseKey      = advapi32.NewProc("RegCloseKey")
	pRegEnumKeyExW    = advapi32.NewProc("RegEnumKeyExW")
	pRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
)

const (
	hkeyLocalMachine = 0x80000002
	keyRead          = 0x20019
	errMoreData      = 234
)

func netApiBufferFree(buf *byte) {
	if buf == nil {
		return
	}
	pNetApiBufferFree.Call(uintptr(unsafe.Pointer(buf)))
}

// lookupNameBySID resolves a SID to its localised group name, which is what
// keeps the Administrators check working on a non-English install.
func lookupNameBySID(sid []byte) string {
	var name [256]uint16
	nameLen := uint32(len(name))
	var domain [256]uint16
	domainLen := uint32(len(domain))
	var use uint32

	r, _, _ := pLookupAccountSID.Call(
		0,
		uintptr(unsafe.Pointer(&sid[0])),
		uintptr(unsafe.Pointer(&name[0])),
		uintptr(unsafe.Pointer(&nameLen)),
		uintptr(unsafe.Pointer(&domain[0])),
		uintptr(unsafe.Pointer(&domainLen)),
		uintptr(unsafe.Pointer(&use)),
	)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(name[:nameLen])
}

// registryKey is a handle to an open registry subtree. Only reading is needed
// here: the profile list, to turn an account name into a profile directory.
type registryKey struct{ handle syscall.Handle }

func registryOpenKey(subkey string) (*registryKey, error) {
	path, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return nil, err
	}
	var h syscall.Handle
	r, _, _ := pRegOpenKeyExW.Call(
		hkeyLocalMachine,
		uintptr(unsafe.Pointer(path)),
		0,
		keyRead,
		uintptr(unsafe.Pointer(&h)),
	)
	if r != 0 {
		return nil, errNotInRegistry
	}
	return &registryKey{handle: h}, nil
}

var errNotInRegistry = errors.New("registry key not found")

func (k *registryKey) Close() {
	pRegCloseKey.Call(uintptr(k.handle))
}

// Subkeys lists the profile entries, which are named by SID rather than by
// account name.
func (k *registryKey) Subkeys() []string {
	var out []string
	buf := make([]uint16, 256)
	for i := uint32(0); ; {
		n := uint32(len(buf))
		r, _, _ := pRegEnumKeyExW.Call(
			uintptr(k.handle),
			uintptr(i),
			0, 0, 0, // name is returned in buf
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&n)),
			0,
		)
		switch r {
		case 0:
			out = append(out, syscall.UTF16ToString(buf[:n]))
			i++
		case errMoreData:
			// The name did not fit. Retry the same index with more room
			// rather than skipping the entry.
			buf = make([]uint16, len(buf)*2)
		default:
			return out
		}
	}
}

// StringValue reads a REG_SZ value.
func (k *registryKey) StringValue(name string) (string, error) {
	valueName, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}

	var typ uint32
	buf := make([]uint16, 512)
	n := uint32(len(buf) * 2)
	r, _, _ := pRegQueryValueExW.Call(
		uintptr(k.handle),
		uintptr(unsafe.Pointer(valueName)),
		0,
		uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&n)),
	)
	if r != 0 {
		return "", errNotInRegistry
	}
	return syscall.UTF16ToString(buf[:n/2]), nil
}
