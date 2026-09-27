//go:build !linux && !darwin

package release

import "errors"

func publishDirectory(string, string) error {
	return errors.New("atomic release publication requires Linux or macOS")
}
