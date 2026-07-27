package cloudhypervisor

// Cloud Hypervisor API operation names are part of the backend protocol.
const (
	apiVMInfo         = "vm.info"
	apiVMSnapshot     = "vm.snapshot"
	apiVMRestore      = "vm.restore"
	apiVMResume       = "vm.resume"
	apiVMPause        = "vm.pause"
	apiVMShutdown     = "vm.shutdown"
	apiVMRemoveDevice = "vm.remove-device"
	apiVMAddNet       = "vm.add-net"
	apiVMAddDisk      = "vm.add-disk"
	apiVMAddFS        = "vm.add-fs"

	backendStateRunning = "Running"
	backendStatePaused  = "Paused"
)
