package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/backend/cloudhypervisor"
	"github.com/kumabox/kumabox/internal/metering"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

func newUsageCommand(opts *rootOptions) *cobra.Command {
	var sinceValue string
	var untilValue string
	cmd := &cobra.Command{
		Use: "usage [VM]", Short: "Show VM compute usage intervals", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			if stores.Metadata != nil {
				defer func() { _ = stores.Metadata.Close() }()
			}
			rt, err := kbruntime.NewWithBackendAndStores(stores, cloudhypervisor.NewBackend(cfg))
			if err != nil {
				return err
			}
			if err := rt.ReconcileMetering(cmd.Context()); err != nil {
				return fmt.Errorf("reconcile metering: %w", err)
			}
			query := metering.Query{}
			if len(args) == 1 {
				query.VMRef = args[0]
			}
			if query.Since, err = parseUsageTime("since", sinceValue); err != nil {
				return err
			}
			if query.Until, err = parseUsageTime("until", untilValue); err != nil {
				return err
			}
			if query.Since != nil && query.Until != nil && !query.Since.Before(*query.Until) {
				return fmt.Errorf("--since must be before --until")
			}
			intervals, err := stores.Metering.Usage(cmd.Context(), query)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), intervals)
		},
	}
	cmd.Flags().StringVar(&sinceValue, "since", "", "include usage ending after this RFC3339 time")
	cmd.Flags().StringVar(&untilValue, "until", "", "include usage starting before this RFC3339 time")
	return cmd
}

func parseUsageTime(name, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("parse --%s as RFC3339: %w", name, err)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}
