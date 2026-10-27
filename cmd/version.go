package cmd

import (
	"fmt"

	"github.com/brogergvhs/kaodoku/internal/service"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show the kaodoku version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("kaodoku version:", service.Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
