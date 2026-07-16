package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

func newHibernateCommand(opts *rootOptions) *cobra.Command {
	var name, consistency string
	cmd := &cobra.Command{
		Use: "hibernate VM", Short: "Durably snapshot a running VM and release its VMM", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if consistency != "crash" && consistency != "fs" {
				return fmt.Errorf("--consistent must be crash or fs")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			result, err := kbruntime.New(cfg).HibernateVM(cmd.Context(), args[0], kbruntime.HibernateOptions{Name: name, Consistency: consistency})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "hibernate snapshot name")
	cmd.Flags().StringVar(&consistency, "consistent", "crash", "consistency: crash or fs")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}
