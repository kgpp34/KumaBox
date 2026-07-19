//go:build !linux

package server

import "errors"

func freezeGuestFilesystems() ([]string, error) {
	return nil, errors.New("filesystem freeze is supported only on Linux")
}

func thawGuestFilesystems() ([]string, error) {
	return nil, errors.New("filesystem thaw is supported only on Linux")
}
