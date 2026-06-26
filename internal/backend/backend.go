package backend

import "github.com/kumabox/kumabox/internal/vmstore"

type Renderer interface {
	RenderConfig(*vmstore.VMRecord) error
}
