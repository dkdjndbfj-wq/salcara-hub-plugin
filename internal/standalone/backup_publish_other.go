//go:build !windows && !linux

package standalone

import "errors"

func publishRestoredDirectory(stage, target string) error {
	return errors.New("atomic restore publication is supported on Linux and Windows")
}
