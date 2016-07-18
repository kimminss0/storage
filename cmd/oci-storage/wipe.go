package main

import (
	"fmt"
	"os"

	"github.com/containers/storage/pkg/mflag"
	"github.com/containers/storage/storage"
)

func wipe(flags *mflag.FlagSet, action string, m storage.Mall, args []string) int {
	err := m.Wipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
		return 1
	}
	return 0
}

func init() {
	commands = append(commands, command{
		names:   []string{"wipe"},
		usage:   "Wipe all layers, images, and containers",
		minArgs: 0,
		action:  wipe,
	})
}
