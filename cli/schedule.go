package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser parses both standard 5-field cron specs and @descriptors
// (@every 24h, @daily, ...).
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// scheduledCommand pairs a saved command with its cron spec.
type scheduledCommand struct {
	Command *Command
	Spec    string
}

// nextRun returns the next execution time after t for a cron spec.
func nextRun(spec string, t time.Time) (time.Time, error) {
	s, err := cronParser.Parse(spec)
	if err != nil {
		return time.Time{}, err
	}
	return s.Next(t), nil
}

// scheduledCommands lists saved commands that declare a schedule.
func scheduledCommands(dir string) ([]scheduledCommand, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]scheduledCommand, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		cmd, err := parseCommand(data)
		if err != nil {
			continue
		}
		if cmd.Schedule == "" {
			continue
		}
		if cmd.Name == "" {
			cmd.Name = name
		}
		out = append(out, scheduledCommand{Command: cmd, Spec: cmd.Schedule})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Command.Name < out[j].Command.Name })
	return out, nil
}

// schedulerState tracks the last run time per command so a manual run --due
// does not re-run commands the daemon already executed.
type schedulerState struct {
	LastRuns map[string]time.Time `json:"last_runs"`
}

func schedulerStatePath() string {
	return filepath.Join(gwStateDir(), "scheduler-state.json")
}

func loadSchedulerState() *schedulerState {
	s := &schedulerState{LastRuns: map[string]time.Time{}}
	if data, err := os.ReadFile(schedulerStatePath()); err == nil {
		_ = json.Unmarshal(data, s)
	}
	if s.LastRuns == nil {
		s.LastRuns = map[string]time.Time{}
	}
	return s
}

func saveSchedulerState(s *schedulerState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(gwStateDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(schedulerStatePath(), data, 0o600)
}

// schedulerStatePaths returns the log and pid files the daemon manages.
func schedulerStatePaths() (logPath, pidPath string) {
	base := gwStateDir()
	return filepath.Join(base, "scheduler.log"), filepath.Join(base, "scheduler.pid")
}

// pidAlive reports whether the process recorded in pidPath is running.
func pidAlive(pidPath string) (int, bool) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return pid, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return pid, false
	}
	return pid, true
}

// runScheduled executes one scheduled command, appending a result line to the
// scheduler log. The command's reply and session events flow through runCommand.
func runScheduled(cfg *Config, cmd *Command) {
	logPath, _ := schedulerStatePaths()
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer logF.Close()
	ts := time.Now().Format(time.RFC3339)
	write := func(msg string) { fmt.Fprintf(logF, "%s [%s] %s\n", ts, cmd.Name, msg) }
	write("start")
	if code := runCommand(cfg, cfg.DefaultAlias, cmd, ""); code != 0 {
		write("failed")
		return
	}
	write("done")
}

// scheduleDaemon is the resident loop started by `gw schedule start`. It fires
// each scheduled command via robfig cron and stops on SIGINT/SIGTERM.
func scheduleDaemon() int {
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	items, err := scheduledCommands(promptsDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	c := cron.New(cron.WithParser(cronParser))
	state := loadSchedulerState()
	registered := 0
	for _, it := range items {
		spec, cmd := it.Spec, it.Command
		if _, err := c.AddFunc(spec, func() {
			runScheduled(cfg, cmd)
			state.LastRuns[cmd.Name] = time.Now()
			_ = saveSchedulerState(state)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "gw: 跳过非法 schedule %s (%q): %v\n", cmd.Name, spec, err)
			continue
		}
		registered++
	}
	c.Start()
	fmt.Printf("scheduler daemon running: %d command(s)\n", registered)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	c.Stop()
	fmt.Println("scheduler daemon stopped")
	return 0
}

// startScheduler launches the daemon in the background, writing pid + log.
func startScheduler() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath, pidPath := schedulerStatePaths()
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logF.Close()
	cmd := exec.Command(exe, "schedule", "daemon")
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
}
