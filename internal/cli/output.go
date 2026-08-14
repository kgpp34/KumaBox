package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/kumabox/kumabox/internal/doctor"
	"github.com/kumabox/kumabox/internal/image"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/vm"
)

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeDoctorText(w io.Writer, report doctor.Report) {
	_, _ = fmt.Fprintf(w, "doctor: %s\n", report.Status)
	for _, check := range report.Checks {
		if check.Code != "" {
			_, _ = fmt.Fprintf(w, "%s: %s (%s): %s\n", check.Status, check.Name, check.Code, check.Message)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s: %s: %s\n", check.Status, check.Name, check.Message)
	}
}

func writeVMTable(w io.Writer, records []*vm.VMRecord) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tSTATE\tOBSERVED\tBACKEND"); err != nil {
		return err
	}
	for _, rec := range records {
		observed := string(rec.ObservedState)
		if observed == "" {
			observed = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", rec.ID, rec.Name, rec.State, observed, rec.Backend); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeVMEventTable(w io.Writer, events []kbruntime.VMStatusEvent, header bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if header {
		if _, err := fmt.Fprintln(tw, "EVENT\tID\tNAME\tSTATE\tOBSERVED\tBACKEND"); err != nil {
			return err
		}
	}
	for _, event := range events {
		record := event.VM
		if record == nil {
			continue
		}
		observed := string(record.ObservedState)
		if observed == "" {
			observed = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			event.Event, record.ID, record.Name, record.State, observed, record.Backend); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeJSONLine(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func writeImageTable(w io.Writer, records []*image.ImageRecord) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tSOURCE\tFORMAT\tPROFILE"); err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			rec.ID,
			rec.Name,
			rec.Source.Type,
			rec.RootDisk.Format,
			rec.OS.Profile,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeNetworkTable(w io.Writer, records []kbnetwork.Record) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tVM\tPROVIDER\tIFACE\tTAP\tMAC\tIPS\tCLEANUP"); err != nil {
		return err
	}
	for _, rec := range records {
		cleanup := "ok"
		if rec.Cleanup.Pending {
			cleanup = "pending"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			rec.ID,
			rec.VMID,
			rec.Provider,
			rec.IfName,
			rec.TAP,
			rec.MAC,
			strings.Join(rec.IPs, ","),
			cleanup,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeVMLogs(w io.Writer, logs *kbruntime.VMLogs) error {
	if len(logs.Files) == 1 {
		_, err := io.WriteString(w, logs.Files[0].Content)
		return err
	}
	for i, file := range logs.Files {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "==> %s <==\n", file.Name); err != nil {
			return err
		}
		if _, err := io.WriteString(w, file.Content); err != nil {
			return err
		}
		if file.Content != "" && file.Content[len(file.Content)-1] != '\n' {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeVMLogChunk(w io.Writer, chunk kbruntime.VMLogChunk, header bool) error {
	if header {
		if _, err := fmt.Fprintf(w, "==> %s <==\n", chunk.Name); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, chunk.Content)
	return err
}
