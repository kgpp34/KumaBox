package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/host"
	"github.com/kumabox/kumabox/layout"
)

// phase is the phase doctor judges against. It moves forward as phases land; it
// is not a flag, because "is this machine ready for what we are building" is a
// property of the build, not of the caller.
const phase = host.PhaseImage

// Options carries the doctor flags.
type Options struct {
	Root string
	JSON bool
	Fix  bool
}

// Handler runs the doctor command.
type Handler struct{}

// Doctor inspects the machine and returns the report.
func (Handler) Doctor(ctx context.Context, options Options) (host.Report, error) {
	cfg, err := config.Load(options.Root)
	if err != nil {
		return host.Report{}, err
	}
	root, err := layout.New(cfg.Root)
	if err != nil {
		return host.Report{}, err
	}

	// --fix is the only thing doctor may change, and it stays inside paths
	// KumaBox owns.
	if options.Fix {
		if err := root.Prepare(); err != nil {
			return host.Report{}, err
		}
	}

	facts, err := host.Collector{}.Collect(ctx, root)
	if err != nil {
		return host.Report{}, err
	}
	return host.Evaluate(facts, phase), nil
}

// NotReady returns an error when the report contains an unmet requirement of
// the current phase.
func (Handler) NotReady(report host.Report) error {
	if !report.Failed() {
		return nil
	}
	return fmt.Errorf("%w: not ready for phase %s", host.ErrNotReady, report.Phase)
}

// Render writes the report to out.
func (Handler) Render(out io.Writer, report host.Report, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}

	fmt.Fprintf(out, "kumabox doctor - machine readiness for phase %s\n\n", report.Phase)
	fmt.Fprintf(out, "host   %s/%s", report.OS, report.Arch)
	if report.Kernel != "" {
		fmt.Fprintf(out, "  kernel %s", report.Kernel)
	}
	fmt.Fprintf(out, "\nroot   %s\n\n", report.Root)

	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "CHECK\tSTATE\tSINCE\tDETAIL")
	for _, check := range report.Checks {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", check.Name, check.State, check.Since, check.Detail)
	}
	if err := table.Flush(); err != nil {
		return err
	}

	for _, check := range report.Checks {
		switch check.State {
		case host.StateMissing, host.StateUnsupported:
			fmt.Fprintf(out, "\n%s: %s\n  fix: %s\n", check.Name, check.Detail, check.Fix)
		}
	}
	return nil
}
