package bridge

import (
	"testing"

	"github.com/kumabox/kumabox/types"
)

func TestOptionsValidate(t *testing.T) {
	for _, test := range []struct {
		name    string
		options Options
		wantErr bool
	}{
		{name: "defaults", options: Options{TAPPrefix: "bt", QueueSize: 512, LeaseDir: t.TempDir()}},
		{name: "empty prefix", options: Options{QueueSize: 512, LeaseDir: t.TempDir()}, wantErr: true},
		{name: "long prefix", options: Options{TAPPrefix: "toolong", QueueSize: 512, LeaseDir: t.TempDir()}, wantErr: true},
		{name: "bad prefix", options: Options{TAPPrefix: "b/", QueueSize: 512, LeaseDir: t.TempDir()}, wantErr: true},
		{name: "queue size", options: Options{TAPPrefix: "bt", LeaseDir: t.TempDir()}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.options.Validate(); (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestProviderType(t *testing.T) {
	provider, err := New(Options{TAPPrefix: "bt", QueueSize: 512, LeaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Type() != types.NetworkBackendBridge {
		t.Fatalf("Type() = %q", provider.Type())
	}
}
