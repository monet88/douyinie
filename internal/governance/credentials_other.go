//go:build !windows

package governance

import (
	"fmt"
	"runtime"

	"github.com/monet88/douyinie/internal/domain"
)

func verifyWindowsCredential(targetName string) (bool, error) {
	return false, fmt.Errorf("%w: os_credential_store is unsupported on platform %s", domain.ErrAuthRequired, runtime.GOOS)
}

func readWindowsCredential(targetName string) (string, error) {
	return "", fmt.Errorf("%w: os_credential_store is unsupported on platform %s", domain.ErrAuthRequired, runtime.GOOS)
}
