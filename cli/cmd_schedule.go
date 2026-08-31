package main

import (
	"fmt"
	"os"
	"time"
)

// cmdSchedule manages the built-in scheduler: list/set/unset scheduled commands
// and start/stop the resident daemon.
func cmdSchedule(args []string) int {
	if len(args) == 0 {
		return scheduleList()
	}
	switch args[0] {
	case "set":
		return scheduleSet(args[1:])
	case "unset":
		return scheduleUnset(args[1:])
	case "run":
		return scheduleRun(args[1:])
	case "start":
		return scheduleStart(args[1:])
	case "stop":
		return scheduleStop(args[1:])
	case "daemon":
		return scheduleDaemon()
	default:
		fmt.Fprintf(os.Stderr, "gw: unknown schedule subcommand %q\n", args[0])
		return 2
	}
}

func scheduleList() int {
	dir := promptsDir()
	items, err := scheduledCommands(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	logPath, pidPath := schedulerStatePaths()
	status := "stopped"
	if pid, running := pidAlive(pidPath); running {
		status = fmt.Sprintf("running (pid %d)", pid)
	}
	fmt.Printf("scheduler: %s (log: %s)\n", status, logPath)
	if len(items) == 0 {
		fmt.Println("no scheduled commands")
		return 0
	}
	now := time.Now()
	for _, it := range items {
		next, err := nextRun(it.Spec, now)
		if err != nil {
			fmt.Printf("  %-20s %-18s (invalid: %v)\n", it.Command.Name, it.Spec, err)
			continue
		}
		fmt.Printf("  %-20s %-18s next %s\n", it.Command.Name, it.Spec, next.Format(time.RFC3339))
	}
	return 0
}

func scheduleSet(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw schedule set <command> <cron>")
		return 2
	}
	name, spec := args[0], args[1]
	if _, err := nextRun(spec, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "gw: 非法 cron 表达式 %q: %v\n", spec, err)
		return 1
	}
	dir := promptsDir()
	data, err := os.ReadFile(commandPath(dir, name))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gw: 命令 %q 不存在(先用 /save 沉淀)\n", name)
		return 1
	}
	cmd, err := parseCommand(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	cmd.Schedule = spec
	if err := writeCommand(dir, name, cmd); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Printf("schedule set: %s → %s\n", name, spec)
	return 0
}

func scheduleUnset(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw schedule unset <command>")
		return 2
	}
	name := args[0]
	dir := promptsDir()
	data, err := os.ReadFile(commandPath(dir, name))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gw: 命令 %q 不存在(先用 /save 沉淀)\n", name)
		return 1
	}
	cmd, err := parseCommand(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	cmd.Schedule = ""
	if err := writeCommand(dir, name, cmd); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Printf("schedule unset: %s\n", name)
	return 0
}

// scheduleRun executes every scheduled command whose next run has passed,
// recording the run time so the daemon will not double-run it.
func scheduleRun(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw schedule run")
		return 2
	}
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	items, err := scheduledCommands(promptsDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	state := loadSchedulerState()
	now := time.Now()
	ran := 0
	for _, it := range items {
		last := state.LastRuns[it.Command.Name]
		next, err := nextRun(it.Spec, last)
		if err != nil || next.After(now) {
			continue // invalid spec or not due yet
		}
		runScheduled(cfg, it.Command)
		state.LastRuns[it.Command.Name] = now
		ran++
	}
	_ = saveSchedulerState(state)
	fmt.Printf("ran %d scheduled command(s)\n", ran)
	return 0
}

func scheduleStart(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw schedule start")
		return 2
	}
	_, pidPath := schedulerStatePaths()
	if pid, running := pidAlive(pidPath); running {
		fmt.Fprintf(os.Stderr, "gw: scheduler already running (pid %d)\n", pid)
		return 1
	}
	if err := startScheduler(); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	logPath, _ := schedulerStatePaths()
	pid, _ := pidAlive(pidPath)
	fmt.Printf("scheduler started (pid %d, log: %s)\n", pid, logPath)
	return 0
}

func scheduleStop(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw schedule stop")
		return 2
	}
	_, pidPath := schedulerStatePaths()
	pid, running := pidAlive(pidPath)
	if !running {
		fmt.Println("no scheduler running")
		return 0
	}
	if proc, err := os.FindProcess(pid); err == nil {
		if err := proc.Signal(os.Interrupt); err != nil {
			_ = proc.Kill()
		}
	}
	_ = os.Remove(pidPath)
	fmt.Printf("scheduler stopped (pid %d)\n", pid)
	return 0
}
