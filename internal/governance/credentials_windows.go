//go:build windows

package governance

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modAdvapi32     = syscall.NewLazyDLL("advapi32.dll")
	procCredReadW   = modAdvapi32.NewProc("CredReadW")
	procCredFree    = modAdvapi32.NewProc("CredFree")
	procCredWriteW  = modAdvapi32.NewProc("CredWriteW")
	procCredDeleteW = modAdvapi32.NewProc("CredDeleteW")
)

const (
	credTypeGeneric    = 1
	credPersistSession = 1
)

func verifyWindowsCredential(targetName string) (bool, error) {
	targetPtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return false, fmt.Errorf("invalid credential target name")
	}

	var pCred uintptr
	r1, _, err := procCredReadW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(credTypeGeneric),
		0,
		uintptr(unsafe.Pointer(&pCred)),
	)

	if r1 != 0 {
		// Credential exists; immediately free allocated structure without exposing secret bytes
		if pCred != 0 {
			procCredFree.Call(pCred)
		}
		return true, nil
	}

	// r1 == 0: Not found or failure
	if errno, ok := err.(syscall.Errno); ok && errno == 1168 { // ERROR_NOT_FOUND
		return false, nil
	}

	return false, nil
}

type winCredentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// writeWindowsCredential is a helper for testing Windows Credential Manager integration.
func writeWindowsCredential(targetName string, secret []byte) error {
	targetPtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return err
	}

	var blobPtr *byte
	if len(secret) > 0 {
		blobPtr = &secret[0]
	}

	cred := winCredentialW{
		Type:               credTypeGeneric,
		TargetName:         targetPtr,
		CredentialBlobSize: uint32(len(secret)),
		CredentialBlob:     blobPtr,
		Persist:            credPersistSession,
	}

	r1, _, err := procCredWriteW.Call(
		uintptr(unsafe.Pointer(&cred)),
		0,
	)
	if r1 == 0 {
		return fmt.Errorf("CredWriteW failed: %w", err)
	}
	return nil
}

// deleteWindowsCredential is a helper for cleaning up credentials created during testing.
func deleteWindowsCredential(targetName string) error {
	targetPtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return err
	}

	r1, _, err := procCredDeleteW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(credTypeGeneric),
		0,
	)
	if r1 == 0 {
		if errno, ok := err.(syscall.Errno); ok && errno == 1168 { // ERROR_NOT_FOUND
			return nil
		}
		return fmt.Errorf("CredDeleteW failed: %w", err)
	}
	return nil
}
