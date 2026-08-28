package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// gatewayConfig is the subset of the gateway's server config that gw up needs
// to bootstrap a local instance and wire the CLI config. It deliberately does
// not import the gateway's internal config package.
type gatewayConfig struct {
	Listen  string `yaml:"listen"`
	Healthz string `yaml:"healthz"`
	Auth    struct {
		Mode     string `yaml:"mode"`
		TokenEnv string `yaml:"token_env"`
	} `yaml:"auth"`
	Teams []struct {
		APIKeys []struct {
			Key string `yaml:"key"`
		} `yaml:"api_keys"`
	} `yaml:"teams"`
	Aliases map[string]yaml.Node `yaml:"aliases"`
	Admin   *struct {
		Enabled  bool   `yaml:"enabled"`
		TokenEnv string `yaml:"token_env"`
	} `yaml:"admin"`
}

// randomToken returns a 16-hex-char admin token for the injected admin block.
func randomToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "local-dev-token"
	}
	return hex.EncodeToString(b)
}

// resolveGatewayConfig returns the gateway config path: CLI arg, GW_GATEWAY_CONFIG,
// ~/gw.yaml, else an error.
func resolveGatewayConfig(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if v := os.Getenv("GW_GATEWAY_CONFIG"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if p := filepath.Join(home, "gw.yaml"); fileExists(p) {
			return p, nil
		}
	}
	return "", errors.New("no gateway config; pass a path, e.g. gw up ~/gw.yaml (or set GW_GATEWAY_CONFIG)")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readGatewayConfig(path string) (*gatewayConfig, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var cfg gatewayConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, string(data), nil
}

// firstAlias returns the first alias name (sorted) for the CLI default.
func firstAlias(aliases map[string]yaml.Node) string {
	names := make([]string, 0, len(aliases))
	for n := range aliases {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "chat"
	}
	return names[0]
}

// inferAPIKey derives the gateway request credential from auth config.
func inferAPIKey(cfg *gatewayConfig) string {
	switch cfg.Auth.Mode {
	case "static":
		return os.Getenv(cfg.Auth.TokenEnv)
	case "api-key":
		for _, t := range cfg.Teams {
			for _, k := range t.APIKeys {
				if strings.TrimSpace(k.Key) != "" {
					return k.Key
				}
			}
		}
	}
	return ""
}

// gatewayStatePaths returns the files gw up/down manage under the CLI state dir.
func gatewayStatePaths() (binDir, runtimeConfig, logPath, pidPath string) {
	base := gwStateDir()
	return filepath.Join(base, "bin"), filepath.Join(base, "gateway.yaml"),
		filepath.Join(base, "gateway.log"), filepath.Join(base, "gateway.pid")
}

// locateGateway finds a gateway executable: GW_GATEWAY_BIN wins, then one
// installed alongside gw by cli/install.sh, then it builds from the
// source tree the CLI binary lives in.
func locateGateway() (string, error) {
	if b := os.Getenv("GW_GATEWAY_BIN"); b != "" {
		return b, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve gw binary: %w", err)
	}
	// Installed next to gw by cli/install.sh.
	if p := filepath.Join(filepath.Dir(exe), "gateway"); fileExists(p) {
		return p, nil
	}
	for _, dir := range []string{"$HOME/.local/bin", "$HOME/bin"} {
		if p := filepath.Join(os.ExpandEnv(dir), "gateway"); fileExists(p) {
			return p, nil
		}
	}
	// Walk up from the gw executable looking for cmd/gateway (source layout).
	for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "cmd", "gateway", "main.go")) {
			binDir, _, _, _ := gatewayStatePaths()
			if err := os.MkdirAll(binDir, 0o700); err != nil {
				return "", err
			}
			bin := filepath.Join(binDir, "gateway")
			// Build from the source root so the go command resolves the right
			// module (the CLI lives in a separate module under the repo).
			build := exec.Command("go", "build", "-o", bin, "./cmd/gateway")
			build.Dir = dir
			build.Stderr = os.Stderr
			if err := build.Run(); err != nil {
				return "", fmt.Errorf("build gateway: %w", err)
			}
			return bin, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", errors.New("gateway binary not found; set GW_GATEWAY_BIN or run gw from the source repo")
}

// waitReady polls the gateway's readyz until healthy.
func waitReady(healthz string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 60; i++ {
		resp, err := client.Get("http://" + healthz + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("gateway did not become ready at %s (log: %s)", healthz, func() string {
		_, _, logPath, _ := gatewayStatePaths()
		return logPath
	}())
}

// startGateway launches the gateway in the background, writing pid + log.
func startGateway(bin, runtimeConfig, token string) error {
	_, _, logPath, pidPath := gatewayStatePaths()
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "-config", runtimeConfig)
	cmd.Env = append(os.Environ(), "GW_DEV_ADMIN_TOKEN="+token)
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		logF.Close()
		return err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return err
	}
	return logF.Close()
}

func gatewayRunning(pidPath string) (int, bool) {
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

func cmdUp(args []string) int {
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw up [config.yaml]")
		return 2
	}
	cfgPath, err := resolveGatewayConfig(firstNonEmpty(args...))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	cfg, raw, err := readGatewayConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	if len(cfg.Aliases) == 0 {
		fmt.Fprintln(os.Stderr, "gw: config has no aliases")
		return 1
	}

	binDir, runtimeConfig, _, pidPath := gatewayStatePaths()
	if _, running := gatewayRunning(pidPath); running {
		fmt.Fprintf(os.Stderr, "gw: gateway already running (pid %d); use `gw down` first\n", func() int {
			p, _ := gatewayRunning(pidPath)
			return p
		}())
		return 1
	}

	// Runtime config = user config + injected admin (only when absent).
	adminToken := ""
	runtimeYAML := raw
	if cfg.Admin == nil || !cfg.Admin.Enabled {
		adminToken = randomToken()
		runtimeYAML += "\n# auto-injected by gw up (admin surface for gw reload)\n" +
			"admin:\n  enabled: true\n  token_env: GW_DEV_ADMIN_TOKEN\n"
	} else if cfg.Admin.TokenEnv != "" {
		// User-provided admin: reuse its token_env's current value.
		adminToken = os.Getenv(cfg.Admin.TokenEnv)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	if err := os.WriteFile(runtimeConfig, []byte(runtimeYAML), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}

	bin, err := locateGateway()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}

	listen := cfg.Listen
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	healthz := cfg.Healthz
	if healthz == "" {
		healthz = listen
	}

	if err := startGateway(bin, runtimeConfig, adminToken); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "gateway starting, waiting for ready...")
	if err := waitReady(healthz); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	pid, _ := gatewayRunning(pidPath)
	fmt.Fprintf(os.Stdout, "gateway running on %s (ops %s), pid %d\n", listen, healthz, pid)

	// Wire up the CLI config so gw works with no further flags.
	cliCfg := &Config{
		GatewayURL:   "http://" + listen,
		AdminURL:     "http://" + healthz,
		APIKey:       inferAPIKey(cfg),
		AdminToken:   adminToken,
		DefaultAlias: firstAlias(cfg.Aliases),
	}
	if err := writeCLIConfig(cliCfg); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Printf("CLI configured → %s\n", func() string {
		p, _ := configPath()
		return p
	}())
	fmt.Printf("\nReady. Try:\n  gw models && gw trans \"hello world\"\n")
	return 0
}

func cmdDown(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "gw: usage: gw down")
		return 2
	}
	_, _, _, pidPath := gatewayStatePaths()
	pid, running := gatewayRunning(pidPath)
	if !running {
		fmt.Println("no gateway running")
		return 0
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		if err := proc.Signal(os.Interrupt); err != nil {
			_ = proc.Kill()
		}
	}
	_ = os.Remove(pidPath)
	fmt.Printf("gateway stopped (pid %d)\n", pid)
	return 0
}

// writeCLIConfig persists the CLI config so the running gateway is the default.
func writeCLIConfig(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
