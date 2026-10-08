package image

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
)

func TestImagesTableHeadersAndAlignedRows(t *testing.T) {
	first, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := types.ParseDigest("sha256:" + strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 14, 16, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	items := []types.Image{
		{
			Names: []string{"demo", "demo-alias-with-a-long-name"}, ManifestDigest: first,
			Platform: types.Platform{OS: "linux", Architecture: "amd64"}, Size: 127600000, CreatedAt: created,
		},
		{ManifestDigest: second, Platform: types.Platform{OS: "linux", Architecture: "arm64"}, Size: 1024, CreatedAt: created},
	}
	var out bytes.Buffer
	if err := writeImagesTable(&out, items); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table = %q", out.String())
	}
	columns := []struct{ header, first, second string }{
		{"NAME", "demo, demo-alias-with-a-long-name", "<none>"},
		{"IMAGE ID", "aaaaaaaaaaaa", "bbbbbbbbbbbb"},
		{"PLATFORM", "linux/amd64", "linux/arm64"},
		{"SIZE", "127.6MB", "1.0kB"},
		{"CREATED", "2026-09-14T08:30:00Z", "2026-09-14T08:30:00Z"},
	}
	for _, column := range columns {
		start := strings.Index(lines[0], column.header)
		if start < 0 || strings.Index(lines[1], column.first) != start || strings.Index(lines[2], column.second) != start {
			t.Fatalf("column %q is missing or misaligned:\n%s", column.header, out.String())
		}
	}
	if strings.ContainsAny(out.String(), "\t\x1b") || strings.Contains(out.String(), first.String()) {
		t.Fatalf("table contains raw tabs, control sequences or full digests: %q", out.String())
	}
	t.Log("\n" + out.String())
}

func TestEmptyImagesTableShowsHeaders(t *testing.T) {
	var out bytes.Buffer
	if err := writeImagesTable(&out, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("empty table = %q", out.String())
	}
	for _, header := range []string{"NAME", "IMAGE ID", "PLATFORM", "SIZE", "CREATED"} {
		if !strings.Contains(out.String(), header) {
			t.Fatalf("missing %q: %q", header, out.String())
		}
	}
}

type failingOutput struct{ err error }

func (w failingOutput) Write([]byte) (int, error) { return 0, w.err }

func TestQueryOutputPreservesWriteErrors(t *testing.T) {
	failure := errors.New("output closed")
	if err := writeImagesTable(failingOutput{failure}, nil); !errors.Is(err, failure) {
		t.Fatalf("table error = %v", err)
	}
	if err := writeJSON(failingOutput{failure}, []imageOutput{}); !errors.Is(err, failure) {
		t.Fatalf("JSON error = %v", err)
	}
}
