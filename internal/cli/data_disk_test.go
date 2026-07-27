package cli

import "testing"

func TestParseDataDisks(t *testing.T) {
	disks, err := parseDataDisks([]string{
		"size=20M,name=db,mount=/var/lib/db,directio=on",
		"size=16M,fstype=none,mount=",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("data disk count = %d", len(disks))
	}
	if disks[0].Name != "db" || disks[0].MountPoint != "/var/lib/db" || disks[0].DirectIO == nil || !*disks[0].DirectIO {
		t.Fatalf("first data disk = %+v", disks[0])
	}
	if disks[1].Name != "data1" || disks[1].MountSet == false || disks[1].Filesystem != "none" {
		t.Fatalf("second data disk = %+v", disks[1])
	}
}

func TestParseDataDisksRejectsMalformedSpec(t *testing.T) {
	for _, value := range []string{"size=8M,name=db", "size=20M,fstype=xfs", "size=20M,name=db,name=other"} {
		if _, err := parseDataDisks([]string{value}); err == nil {
			t.Fatalf("parseDataDisks(%q) error = nil", value)
		}
	}
}
