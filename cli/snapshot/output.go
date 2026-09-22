package snapshot

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/kumabox/kumabox/types"
)

type output struct {
	ID               string       `json:"id"`
	Name             string       `json:"name,omitempty"`
	Description      string       `json:"description,omitempty"`
	SandboxID        string       `json:"sandbox_id"`
	SourceGeneration uint64       `json:"source_generation"`
	ImageDigest      string       `json:"image_digest"`
	VMM              string       `json:"vmm"`
	Config           configOutput `json:"config"`
	Size             int64        `json:"size"`
	CreatedAt        time.Time    `json:"created_at"`
}

type configOutput struct {
	Name        string `json:"name"`
	CPUs        uint32 `json:"cpus"`
	Memory      int64  `json:"memory"`
	Storage     int64  `json:"storage"`
	NICs        int    `json:"nics"`
	NetworkName string `json:"network_name,omitempty"`
}

func result(snapshot types.Snapshot) output {
	return output{
		ID: snapshot.ID.String(), Name: snapshot.Name, Description: snapshot.Description,
		SandboxID: snapshot.SandboxID.String(), SourceGeneration: snapshot.SourceGeneration,
		ImageDigest: snapshot.ImageDigest.String(), VMM: string(snapshot.VMM),
		Config: configOutput{
			Name: snapshot.Config.Name, CPUs: snapshot.Config.CPUs, Memory: snapshot.Config.Memory,
			Storage: snapshot.Config.Storage, NICs: snapshot.Config.NICs, NetworkName: snapshot.Config.NetworkName,
		},
		Size: snapshot.Size, CreatedAt: snapshot.CreatedAt.UTC(),
	}
}

func writeJSON(writer io.Writer, snapshot types.Snapshot) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result(snapshot))
}

func writeResult(writer io.Writer, snapshot types.Snapshot, asJSON bool) error {
	if asJSON {
		return writeJSON(writer, snapshot)
	}
	_, err := fmt.Fprintln(writer, snapshot.ID)
	return err
}

func writeListJSON(writer io.Writer, snapshots []types.Snapshot) error {
	results := make([]output, 0, len(snapshots))
	for _, snapshot := range snapshots {
		results = append(results, result(snapshot))
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func writeTable(writer io.Writer, snapshots []types.Snapshot) error {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "SNAPSHOT ID\tNAME\tSANDBOX ID\tCPUS\tMEMORY\tSIZE\tDESCRIPTION\tCREATED"); err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			snapshot.ID, snapshot.Name, snapshot.SandboxID, snapshot.Config.CPUs,
			formatIECBytes(snapshot.Config.Memory), formatIECBytes(snapshot.Size), snapshot.Description,
			snapshot.CreatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

func formatIECBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%dB", size)
	}
	value := float64(size)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"} {
		value /= 1024
		if value < 1024 || unit == "EiB" {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
	}
	return fmt.Sprintf("%dB", size)
}
