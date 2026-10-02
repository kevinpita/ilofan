package cli

import (
	"encoding/json"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kevinpita/ilofan/internal/ilofan"
)

const defaultConfigPath = "/etc/ilofan/config.json"

// NewCommand builds the ilofan command tree.
func NewCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "ilofan",
		Short:         "Control iLO fans with a temperature-based fan curve",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	var setupPath string
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Ask for the iLO details and write the config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetup(setupPath, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	setup.Flags().StringVar(&setupPath, "config", defaultConfigPath, "Config file to write")
	var daemonPath string
	daemon := &cobra.Command{
		Use:   "daemon",
		Short: "Run the controller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := ilofan.LoadConfig(daemonPath)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return ilofan.RunDaemon(ctx, cfg)
		},
	}
	daemon.Flags().StringVar(&daemonPath, "config", defaultConfigPath, "Config file")
	var socket string
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show sensors, fans and the active override",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := call(socket, http.MethodGet, "/status")
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
			}
			printReport(cmd.OutOrStdout(), r)
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "Print JSON")
	set := &cobra.Command{
		Use:   "set PERCENT",
		Short: "Hold fans at PERCENT or more (temperatures may raise it)",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return err
			}
			_, err := ilofan.ParsePercent(args[0])
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return post(socket, "/set?percent="+args[0], cmd.OutOrStdout())
		},
	}
	auto := &cobra.Command{
		Use:   "auto",
		Short: "Follow the fan curve",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return post(socket, "/auto", cmd.OutOrStdout())
		},
	}
	release := &cobra.Command{
		Use:   "release",
		Short: "Return fans to iLO firmware control",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return post(socket, "/release", cmd.OutOrStdout())
		},
	}
	for _, cmd := range []*cobra.Command{status, set, auto, release} {
		cmd.Flags().StringVar(&socket, "socket", "/run/ilofan/ilofan.sock", "Control socket")
	}
	root.AddCommand(setup, daemon, status, set, auto, release)
	return root
}
