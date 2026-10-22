package types

import "testing"

func TestCPUPolicyResolve(t *testing.T) {
	for _, test := range []struct {
		name  string
		input CPUPolicy
		want  CPUPolicy
	}{
		{"defaults", CPUPolicy{}, CPUPolicy{Weight: 2, QuotaUS: 200_000, PeriodUS: 100_000, BurstUS: 200_000}},
		{"strict", CPUPolicy{BurstUS: -1}, CPUPolicy{Weight: 2, QuotaUS: 200_000, PeriodUS: 100_000, BurstUS: -1}},
		{"custom", CPUPolicy{Weight: 30, QuotaUS: 75_000, PeriodUS: 50_000, BurstUS: 5_000, CPUSet: "2-3"}, CPUPolicy{Weight: 30, QuotaUS: 75_000, PeriodUS: 50_000, BurstUS: 5_000, CPUSet: "2-3"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.input.Resolve(2)
			if err != nil || got != test.want {
				t.Fatalf("resolved = %+v, %v; want %+v", got, err, test.want)
			}
			if again, err := got.Resolve(2); err != nil || again != got {
				t.Fatalf("resolution is not idempotent: %+v, %v", again, err)
			}
		})
	}
	for _, invalid := range []CPUPolicy{
		{Weight: 10_001},
		{PeriodUS: 999},
		{PeriodUS: 1_000_001},
		{QuotaUS: -1},
		{QuotaUS: 999},
		{BurstUS: -2},
		{BurstUS: 200_001},
		{CPUSet: "auto"},
		{CPUSet: "-1"},
		{CPUSet: "3-1"},
		{CPUSet: "1,,2"},
		{CPUSet: "4294967296"},
	} {
		if _, err := invalid.Resolve(2); err == nil {
			t.Errorf("accepted invalid policy %+v", invalid)
		}
	}
}

func TestCPUSetContainsRangesWithoutExpanding(t *testing.T) {
	for _, test := range []struct {
		available, requested string
		want                 bool
	}{
		{"0-7", "1,3-5", true},
		{"0-1,4-5", "1-4", false},
		{"3,0-2,4", "0-4", true},
		{"0-4294967295", "4294967295", true},
		{"", "0", false},
		{"0-3", "", true},
	} {
		got, err := CPUSetContains(test.available, test.requested)
		if err != nil || got != test.want {
			t.Errorf("contains(%q, %q) = %t, %v", test.available, test.requested, got, err)
		}
	}
}

func TestMemoryOptionsAndHostPolicyAreIndependent(t *testing.T) {
	base := SandboxConfig{Name: "resources", CPUs: 2, Memory: DefaultSandboxMemory, Storage: DefaultSandboxStorage}
	other := base
	other.CPUPolicy = CPUPolicy{Weight: 50, CPUSet: "3-4"}
	if !base.SameGuest(other) {
		t.Fatal("host scheduling changed the guest shape")
	}
	for _, option := range []string{"hugepages", "shared-memory"} {
		config := base
		config.Mergeable = true
		config.HugePages = option == "hugepages"
		config.SharedMemory = option == "shared-memory"
		if err := config.Validate(); err == nil {
			t.Errorf("accepted KSM with %s", option)
		}
	}
	other.NoBalloon = true
	if base.SameGuest(other) {
		t.Fatal("balloon device change was ignored by guest-shape comparison")
	}
}
