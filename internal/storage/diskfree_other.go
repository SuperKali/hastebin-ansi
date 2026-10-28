//go:build !(linux || darwin || freebsd)

package storage

import "errors"

// diskFree is not available here, so the free space floor is off
func diskFree(string) (int64, error) {
	return 0, errors.ErrUnsupported
}
