package cmd

import (
	"fmt"

	"github.com/brogergvhs/kaodoku/internal/config"

	"github.com/spf13/cobra"
)

var configRenameCmd = &cobra.Command{
	Use:   "rename <old_label> <new_label>",
	Short: "Rename an existing labeled config (<old_label> <new_label>)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		oldLabel := args[0]
		newLabel := args[1]

		if err := config.RenameConfig(oldLabel, newLabel); err != nil {
			return err
		}
		fmt.Printf("Renamed config %q -> %q\n", oldLabel, newLabel)

		return nil
	},
}

func init() {
	configCmd.AddCommand(configRenameCmd)
}
