package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
)

func newHibernateCommand(opts *rootOptions) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use: "hibernate VM", Short: "Durably snapshot a running VM and release its VMM", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			result, err := rt.HibernateVM(cmd.Context(), args[0], kbruntime.HibernateOptions{Name: name})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "hibernate snapshot name")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}
