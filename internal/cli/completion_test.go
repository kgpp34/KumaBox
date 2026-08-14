package cli

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/vm"
)

func TestCompletionGeneratesSupportedShells(t *testing.T) {
	t.Parallel()

	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		shell := shell
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			cmd := NewRootCommand()
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetArgs([]string{"completion", shell})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if output.Len() == 0 || !strings.Contains(strings.ToLower(output.String()), "kumabox") {
				t.Fatalf("%s completion output is empty or invalid", shell)
			}
		})
	}
}

func TestResourceCompletionReadsVMNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	store := vm.New(rootDir)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.Create(vm.CreateRequest{
			Name: name, RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
			RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	root := newTestRootCommand(rootDir)
	inspect, _, err := root.Find([]string{"inspect"})
	if err != nil {
		t.Fatal(err)
	}
	candidates, directive := inspect.ValidArgsFunction(inspect, nil, "a")
	if directive != cobra.ShellCompDirectiveNoFileComp || !slices.Equal(candidates, []string{"alpha"}) {
		t.Fatalf("completion candidates=%v directive=%v", candidates, directive)
	}
}

func TestCommandResourceKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		use   string
		index int
		want  string
		ok    bool
	}{
		{use: "restore VM SNAPSHOT", index: 0, want: "vm", ok: true},
		{use: "restore VM SNAPSHOT", index: 1, want: "snapshot", ok: true},
		{use: "start VM [VM...]", index: 4, want: "vm", ok: true},
		{use: "exec VM -- CMD [ARG...]", index: 1, ok: false},
	}
	for _, test := range tests {
		got, ok := commandResourceKind(test.use, test.index)
		if got != test.want || ok != test.ok {
			t.Fatalf("commandResourceKind(%q, %d) = %q, %t", test.use, test.index, got, ok)
		}
	}
}
