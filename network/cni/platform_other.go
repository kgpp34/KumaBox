//go:build !linux

package cni

import (
	"context"
	"errors"
)

var errPlatformUnsupported = errors.New("CNI network namespace operations require Linux")

type unsupportedPlatform struct{}

func newPlatform() platform { return unsupportedPlatform{} }

func (unsupportedPlatform) EnsureNamespace(string, string) (bool, error) {
	return false, errPlatformUnsupported
}

func (unsupportedPlatform) RemoveNamespace(context.Context, string) error {
	return errPlatformUnsupported
}

func (unsupportedPlatform) NamespaceExists(string) error { return errPlatformUnsupported }

func (unsupportedPlatform) SetupRedirect(string, string, string, int, string) (string, error) {
	return "", errPlatformUnsupported
}

func (unsupportedPlatform) DeleteTAP(string, string) error { return errPlatformUnsupported }

func (unsupportedPlatform) SetLinkState(string, []string, bool) error {
	return errPlatformUnsupported
}

func (unsupportedPlatform) VerifyTAP(string, string) error { return errPlatformUnsupported }
