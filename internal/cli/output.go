package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/kumabox/kumabox/internal/doctor"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeDoctorText(w io.Writer, report doctor.Report) {
	fmt.Fprintf(w, "doctor: %s\n", report.Status)
	for _, check := range report.Checks {
		if check.Code != "" {
			fmt.Fprintf(w, "%s: %s (%s): %s\n", check.Status, check.Name, check.Code, check.Message)
			continue
		}
		fmt.Fprintf(w, "%s: %s: %s\n", check.Status, check.Name, check.Message)
	}
}

func writeVMTable(w io.Writer, records []*vmstore.VMRecord) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tSTATE\tBACKEND"); err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", rec.ID, rec.Name, rec.State, rec.Backend); err != nil {
			return err
		}
	}
	return tw.Flush()
}
