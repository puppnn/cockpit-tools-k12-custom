//go:build !windows

package auth

import "os"

func replaceK12SessionStateFile(source, destination string) error {
	return os.Rename(source, destination)
}
