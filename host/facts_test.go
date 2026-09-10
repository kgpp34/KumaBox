package host

import (
	"encoding/json"
	"strings"
	"testing"
)

// linuxFacts is a machine that satisfies every phase up to S2.
func linuxFacts() Facts {
	return Facts{
		OS:     "linux",
		Arch:   "amd64",
		Kernel: "6.8.0-45-generic",
		Root: Root{
			Path:       "/var/lib/kumabox",
			Exists:     true,
			Writable:   true,
			TotalBytes: 200 << 30,
			FreeBytes:  120 << 30,
		},
		KVM: KVM{Device: KVMPath, Present: true, Readable: true},
		VMM: Binary{
			Name:    "cloud-hypervisor",
			Path:    "/usr/local/bin/cloud-hypervisor",
			Version: "Cloud Hypervisor v53.0",
			Found:   true,
		},
		CNI: CNI{
			PluginBinDir: DefaultCNIPluginDir,
			ConfigDir:    DefaultCNIConfigDir,
			PluginsFound: true,
			ConfigFound:  true,
		},
		CgroupV2: true,
	}
}

// laptopFacts is a development laptop: no KVM, no Cloud Hypervisor.
func laptopFacts() Facts {
	facts := linuxFacts()
	facts.OS = "darwin"
	facts.Arch = "arm64"
	facts.KVM = KVM{Device: KVMPath}
	facts.VMM = Binary{Name: "cloud-hypervisor"}
	facts.CNI = CNI{PluginBinDir: DefaultCNIPluginDir, ConfigDir: DefaultCNIConfigDir}
	facts.CgroupV2 = false
	return facts
}

func checkNamed(t *testing.T, report Report, name string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("report has no check %q", name)
	return Check{}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		facts      Facts
		phase      Phase
		check      string
		want       State
		wantFailed bool
	}{
		{
			name:  "a ready linux node passes the image phase",
			facts: linuxFacts(),
			phase: PhaseImage,
			check: "root-directory",
			want:  StateOK,
		},
		{
			name:       "a missing root fails the image phase",
			facts:      withRoot(linuxFacts(), Root{Path: "/var/lib/kumabox", TotalBytes: 200 << 30, FreeBytes: 120 << 30}),
			phase:      PhaseImage,
			check:      "root-directory",
			want:       StateMissing,
			wantFailed: true,
		},
		{
			name:       "a read-only root fails the image phase",
			facts:      withRoot(linuxFacts(), Root{Path: "/var/lib/kumabox", Exists: true, TotalBytes: 200 << 30, FreeBytes: 120 << 30}),
			phase:      PhaseImage,
			check:      "root-directory",
			want:       StateMissing,
			wantFailed: true,
		},
		{
			name:       "too little free space fails the image phase",
			facts:      withRoot(linuxFacts(), Root{Path: "/var/lib/kumabox", Exists: true, Writable: true, TotalBytes: 200 << 30, FreeBytes: 1 << 30}),
			phase:      PhaseImage,
			check:      "root-space",
			want:       StateMissing,
			wantFailed: true,
		},
		{
			name:  "kvm is not required while the image phase is the target",
			facts: laptopFacts(),
			phase: PhaseImage,
			check: "kvm",
			want:  StateNotRequired,
		},
		{
			name:       "kvm is required in the sandbox phase",
			facts:      laptopFacts(),
			phase:      PhaseSandbox,
			check:      "kvm",
			want:       StateUnsupported,
			wantFailed: true,
		},
		{
			name:       "an old cloud hypervisor is rejected",
			facts:      withVMMVersion(linuxFacts(), "Cloud Hypervisor v42.0"),
			phase:      PhaseSandbox,
			check:      "cloud-hypervisor",
			want:       StateMissing,
			wantFailed: true,
		},
		{
			name:  "cni is satisfied on a full node",
			facts: linuxFacts(),
			phase: PhaseNetwork,
			check: "cni",
			want:  StateOK,
		},
		{
			name:       "cni failure blocks the network phase",
			facts:      withCNI(linuxFacts(), false, false),
			phase:      PhaseNetwork,
			check:      "cni",
			want:       StateMissing,
			wantFailed: true,
		},
		{
			name:       "cgroup v2 is not required before the convergence phase",
			facts:      withCgroupV2(linuxFacts(), false),
			phase:      PhaseNetwork,
			check:      "cgroup-v2",
			want:       StateNotRequired,
			wantFailed: false,
		},
		{
			name:       "cgroup v2 failure blocks the convergence phase",
			facts:      withCgroupV2(linuxFacts(), false),
			phase:      PhaseConvergence,
			check:      "cgroup-v2",
			want:       StateMissing,
			wantFailed: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report := Evaluate(test.facts, test.phase)
			if got := checkNamed(t, report, test.check).State; got != test.want {
				t.Errorf("check %s state = %s, want %s", test.check, got, test.want)
			}
			if got := report.Failed(); got != test.wantFailed {
				t.Errorf("report.Failed() = %v, want %v (%+v)", got, test.wantFailed, report.Checks)
			}
		})
	}
}

func TestEvaluateNeverFailsOnLaterPhases(t *testing.T) {
	t.Parallel()

	// A laptop that satisfies nothing beyond the root must still pass the image
	// phase: requirements are judged against the phase being worked on.
	facts := withRoot(laptopFacts(), Root{
		Path: "/home/dev/.kumabox", Exists: true, Writable: true,
		TotalBytes: 500 << 30, FreeBytes: 200 << 30,
	})
	if report := Evaluate(facts, PhaseImage); report.Failed() {
		t.Fatalf("the image phase must not fail on later-phase requirements: %+v", report.Checks)
	}
}

func TestStatesAndPhasesRenderAsTextInJSON(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(Evaluate(linuxFacts(), PhaseImage))
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	text := string(encoded)
	for _, want := range []string{`"phase":"S1"`, `"state":"ok"`, `"since":"S1"`} {
		if !strings.Contains(text, want) {
			t.Errorf("JSON report missing %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"state":0`) {
		t.Errorf("states must never be encoded as integers:\n%s", text)
	}
}

func TestMajorOf(t *testing.T) {
	t.Parallel()

	tests := map[string]int{
		"Cloud Hypervisor v43.0": 43,
		"cloud-hypervisor 53.1":  53,
		"43.0":                   43,
		"v100":                   100,
		"":                       0,
		"no digits here":         0,
	}
	for input, want := range tests {
		if got := MajorOf(input); got != want {
			t.Errorf("MajorOf(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := map[uint64]string{
		512:      "512 B",
		1024:     "1 KiB",
		1536:     "1.5 KiB",
		10 << 30: "10 GiB",
	}
	for input, want := range tests {
		if got := HumanBytes(input); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", input, got, want)
		}
	}
}

func withRoot(facts Facts, root Root) Facts {
	facts.Root = root
	return facts
}

func withVMMVersion(facts Facts, version string) Facts {
	facts.VMM.Version = version
	facts.VMM.Found = true
	facts.VMM.Path = "/usr/local/bin/cloud-hypervisor"
	return facts
}

func withCNI(facts Facts, plugins, config bool) Facts {
	facts.CNI.PluginsFound = plugins
	facts.CNI.ConfigFound = config
	return facts
}

func withCgroupV2(facts Facts, mounted bool) Facts {
	facts.CgroupV2 = mounted
	return facts
}
