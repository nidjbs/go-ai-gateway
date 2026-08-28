package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func cmdReload(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	path := fs.String("path", "", "config file path (default: gateway startup path)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := NewClient(cfg).Reload(context.Background(), *path); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Println("reloaded")
	return 0
}

func cmdStatus(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	if err := NewClient(cfg).Status(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

func cmdUsage(args []string) int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	alias := fs.String("alias", "", "filter by alias")
	from := fs.String("from", "", "start time (RFC3339)")
	to := fs.String("to", "", "end time (RFC3339)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	body, err := NewClient(cfg).Usage(context.Background(), *alias, *from, *to)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Println(body)
	return 0
}
