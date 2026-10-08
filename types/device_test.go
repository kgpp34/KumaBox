package types

import "testing"

func TestNormalizeFileShare(t *testing.T) {
	share, err := NormalizeFileShare(FileShare{Socket: "/run/virtiofsd.sock", Tag: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if share.NumQueues != DefaultFileShareQueues || share.QueueSize != DefaultFileShareQueueSize {
		t.Fatalf("file share defaults = %+v", share)
	}
	for _, invalid := range []FileShare{
		{Socket: "relative.sock", Tag: "data"},
		{Socket: "/run/virtiofsd.sock", Tag: "bad/tag"},
		{Socket: "/run/virtiofsd.sock", Tag: "data", NumQueues: -1},
		{Socket: "/run/virtiofsd.sock", Tag: "data", QueueSize: -1},
	} {
		if _, err := NormalizeFileShare(invalid); err == nil {
			t.Fatalf("accepted invalid file share %+v", invalid)
		}
	}
}
