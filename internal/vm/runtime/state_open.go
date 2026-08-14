package runtime

import (
	"github.com/kumabox/kumabox/internal/state"
)

func openStateWithVM(rootDir string, vmState state.VMState) state.Set {
	data := state.OpenJSON(rootDir)
	data.VM = vmState
	return data
}
