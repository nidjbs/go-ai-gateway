package main

import (
	"context"
	"fmt"
	"os"
)

func cmdModels(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	names, err := NewClient(cfg).Models(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return 0
}
