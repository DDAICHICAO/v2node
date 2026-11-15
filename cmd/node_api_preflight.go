package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wyx2685/v2node/node"
)

func init() {
	var path string
	preflight := &cobra.Command{
		Use:          "node-api-preflight",
		Short:        "升级前验证全部节点身份，全部成功后保存密钥迁移",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := node.PreflightNodeAPIUpgrade(context.Background(), path)
			output := struct {
				node.NodeAPICredentialMigrationReport
				Ready bool `json:"upgrade_preflight_ready"`
			}{report, err == nil}
			if outputErr := json.NewEncoder(cmd.OutOrStdout()).Encode(output); outputErr != nil {
				return fmt.Errorf("credential_report_failed")
			}
			return err
		},
	}
	preflight.Flags().StringVarP(&path, "config", "c", "/etc/v2node/config.json", "config file path")
	command.AddCommand(preflight)
}
