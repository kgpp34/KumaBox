package cgroup

import "testing"

func TestNewRequiresParentBelowUnifiedRoot(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		want   string
		valid  bool
	}{
		{name: "default", want: DefaultParent, valid: true},
		{name: "nested", parent: Root + "/tenant/kumabox.slice", want: Root + "/tenant/kumabox.slice", valid: true},
		{name: "root itself", parent: Root},
		{name: "outside root", parent: "/tmp/kumabox.slice"},
		{name: "relative", parent: "kumabox.slice"},
		{name: "escaped", parent: Root + "/../outside"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, err := New(test.parent)
			if !test.valid {
				if err == nil {
					t.Fatalf("New(%q) accepted invalid parent", test.parent)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if manager.Parent() != test.want {
				t.Fatalf("Parent() = %q, want %q", manager.Parent(), test.want)
			}
		})
	}
	var manager *Manager
	if manager.Parent() != "" {
		t.Fatalf("nil manager parent = %q", manager.Parent())
	}
}
