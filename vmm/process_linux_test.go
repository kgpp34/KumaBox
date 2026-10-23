//go:build linux

package vmm

import (
	"errors"
	"testing"
)

func TestCommandLineMatchesDistinguishesPendingIdentity(t *testing.T) {
	const socket = "/run/kumabox/api.sock"
	if match, err := commandLineMatches(nil, "cloud-hypervisor", socket); match || !errors.Is(err, ErrProcessIdentityPending) {
		t.Fatalf("empty command line = %v, %v; want pending identity", match, err)
	}
	if match, err := commandLineMatches([]byte("cloud-hypervisor\x00--api-socket\x00"+socket+"\x00"), "cloud-hypervisor", socket); err != nil || !match {
		t.Fatalf("complete command line = %v, %v; want match", match, err)
	}
	if match, err := commandLineMatches([]byte("other-vmm\x00--api-socket\x00"+socket+"\x00"), "cloud-hypervisor", socket); err != nil || match {
		t.Fatalf("foreign command line = %v, %v; want mismatch", match, err)
	}
}

func TestCommandLineMatchesFirecrackerSocketIdentity(t *testing.T) {
	const socket = "/run/kumabox/api.sock"
	for _, test := range []struct {
		args string
		want bool
	}{
		{"firecracker\x00--api-sock\x00" + socket + "\x00", true},
		{"firecracker\x00--api-sock\x00/foreign/api.sock\x00", false},
		{"firecracker\x00--api-sock\x00" + socket + "\x00--api-socket\x00" + socket + "\x00", false},
	} {
		if got, err := commandLineMatches([]byte(test.args), "firecracker", socket); err != nil || got != test.want {
			t.Fatalf("identity: %v %v", got, err)
		}
	}
}
