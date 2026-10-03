package sandbox

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/storage"
)

func TestStatusCommandShowsCreatedSandboxAsJSON(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	seedImage(t, roots)
	installFakeMKFS(t, base)
	id := executeCreate(t, roots, "box")
	command := NewStatusCommand(func() config.Config { return sandboxTestConfig(roots) })
	command.SetArgs([]string{"box", "--json"})
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	var statuses []statusOutput
	if err := json.Unmarshal(output.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].ID != id.String() || statuses[0].State != "created" || statuses[0].Stale {
		t.Fatalf("status = %+v", statuses)
	}
}
