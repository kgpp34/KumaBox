package cloudhypervisor

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/kumabox/kumabox/vmm"
)

const diskQueueSize = 512

// buildArgs renders one direct-boot Cloud Hypervisor command. Disk attachment
// order remains base-to-top then COW; only the initramfs cmdline reverses layers.
func buildArgs(plan vmm.LaunchPlan, apiSocket, vsock string) []string {
	maximumCPUs := max(runtime.NumCPU(), int(plan.CPUs))
	args := []string{
		"--api-socket", apiSocket,
		"--cpus", fmt.Sprintf("boot=%d,max=%d", plan.CPUs, maximumCPUs),
		"--memory", fmt.Sprintf("size=%d", plan.Memory),
		"--disk",
	}
	for _, disk := range plan.Disks {
		parts := []string{
			"path=" + disk.Path,
			"image_type=raw",
			fmt.Sprintf("num_queues=%d", plan.CPUs),
			fmt.Sprintf("queue_size=%d", diskQueueSize),
			"serial=" + disk.Serial,
		}
		if disk.ReadOnly {
			parts = append(parts, "readonly=on")
		} else {
			parts = append(parts, "direct=on", "sparse=on")
		}
		args = append(args, strings.Join(parts, ","))
	}
	if len(plan.Network.Interfaces) > 0 {
		args = append(args, "--net")
		for _, networkInterface := range plan.Network.Interfaces {
			args = append(args, strings.Join([]string{
				"tap=" + networkInterface.TAP,
				"mac=" + networkInterface.MAC,
				fmt.Sprintf("num_queues=%d", networkInterface.Queues),
				fmt.Sprintf("queue_size=%d", networkInterface.QueueSize),
				"offload_tso=on",
				"offload_ufo=on",
				"offload_csum=on",
			}, ","))
		}
	}
	args = append(args,
		"--kernel", plan.Kernel,
		"--initramfs", plan.Initrd,
		"--cmdline", plan.Cmdline,
		"--rng", "src=/dev/urandom",
		"--watchdog",
		"--balloon", fmt.Sprintf("size=%d,deflate_on_oom=on,free_page_reporting=on", plan.Memory/4),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", vmm.VsockGuestCID, vsock),
		"--serial", "off",
		"--console", "pty",
	)
	return args
}
