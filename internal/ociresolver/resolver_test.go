// SPDX-License-Identifier: MIT

package ociresolver

import "testing"

func TestParsePlatform(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    Platform
		wantErr bool
	}{
		{
			name:  "linux amd64",
			value: "linux/amd64",
			want:  Platform{OS: "linux", Architecture: "amd64"},
		},
		{
			name:  "linux arm variant",
			value: "linux/arm/v7",
			want:  Platform{OS: "linux", Architecture: "arm", Variant: "v7"},
		},
		{
			name:    "missing arch",
			value:   "linux",
			wantErr: true,
		},
		{
			name:    "unsupported os",
			value:   "windows/amd64",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePlatform(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePlatform returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParsePlatform = %+v, want %+v", got, tt.want)
			}
		})
	}
}
