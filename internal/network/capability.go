package network

import (
	"os"
	"os/exec"
	"runtime"
)

type CapabilityReport struct {
	OS          string
	TunDevice   bool
	IPCommand   string
	Iptables    string
	Nft         string
	RootUser    bool
	NATBackend  string
	Unavailable []string
}

func CheckCapabilities(natBackend string) CapabilityReport {
	report := CapabilityReport{
		OS:         runtime.GOOS,
		NATBackend: natBackend,
		RootUser:   os.Geteuid() == 0,
	}
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/net/tun"); err == nil {
			report.TunDevice = true
		}
	}
	if path, err := exec.LookPath("ip"); err == nil {
		report.IPCommand = path
	}
	if path, err := exec.LookPath("iptables"); err == nil {
		report.Iptables = path
	}
	if path, err := exec.LookPath("nft"); err == nil {
		report.Nft = path
	}

	if report.OS != "linux" {
		report.Unavailable = append(report.Unavailable, "linux")
	}
	if !report.TunDevice {
		report.Unavailable = append(report.Unavailable, "tun")
	}
	if report.IPCommand == "" {
		report.Unavailable = append(report.Unavailable, "ip")
	}
	if natBackend != NATBackendNone && report.Iptables == "" && report.Nft == "" {
		report.Unavailable = append(report.Unavailable, "nat")
	}
	if !report.RootUser {
		report.Unavailable = append(report.Unavailable, "root")
	}
	return report
}
