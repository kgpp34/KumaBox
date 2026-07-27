package runtime

// networkCoordinator owns host-side network allocation, provider state, and
// rollback. It embeds Runtime only to share the already-injected stores and
// configuration; VM lifecycle code reaches network operations through this
// boundary instead of implementing provider details itself.
type networkCoordinator struct {
	*Runtime
}

func (r *Runtime) initNetworkCoordinator() {
	r.network = &networkCoordinator{Runtime: r}
	r.storage = &storageCoordinator{Runtime: r}
}
