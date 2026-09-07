package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdEval(args []string) int {
	if len(args) == 0 {
		evalUsage()
		return 2
	}
	switch args[0] {
	case "snapshot":
		return cmdEvalSnapshot(args[1:])
	case "quality":
		return cmdEvalQuality(args[1:])
	default:
		evalUsage()
		return 2
	}
}

func evalUsage() {
	fmt.Fprint(os.Stderr, `gw eval - 黄金回归(mock 快照) + 真实多轮评测(quality, 含 judge 硬门禁)

用法:
  gw eval snapshot <cases-dir> [--golden <dir>] [--filter <glob>] [--record] [--update] [--report <file>]
  gw eval quality  <cases-dir> [--filter <glob>] [--report <file>]

退出码: 0 全过; 1 有 FAIL/DIFF/NEEDS_REVIEW/ERROR; 2 用法错。
`)
}

// evalSnapshot runs every mock case against its own deterministic fake and
// compares against goldenRoot/<name>. record 补缺失 golden; update 重写已有。
func evalSnapshot(cases []*Case, goldenRoot string, record, update bool) Report {
	rep := Report{}
	for _, c := range cases {
		if c.Strategy != StrategyMock {
			continue
		}
		res := CaseResult{Name: c.Name}
		f := newEvalFake(c.Stub, c.Fallback)
		out, err := runCase(c, RunOpts{Gateway: f.URL(), Timeout: c.timeout()})
		f.Close()
		if err != nil {
			res.Status = StatusError
			res.Detail = err.Error()
			rep.Results = append(rep.Results, res)
			continue
		}
		snap := snapshotOf(c, out)
		want, ok := readGolden(goldenRoot, c.Name)
		switch {
		case !ok && record:
			if err := writeGolden(goldenRoot, c.Name, snap); err != nil {
				res.Status = StatusError
				res.Detail = err.Error()
			} else {
				res.Status = StatusPass
				res.Detail = "golden 已记录(--record)"
			}
		case !ok:
			res.Status = StatusFail
			res.Detail = "缺少 golden,先运行: gw eval snapshot <dir> --record"
		case update:
			if err := writeGolden(goldenRoot, c.Name, snap); err != nil {
				res.Status = StatusError
				res.Detail = err.Error()
			} else {
				res.Status = StatusPass
				res.Detail = "golden 已更新(--update)"
			}
		default:
			if diff := compareSnap(snap, want); diff != "" {
				res.Status = StatusDiff
				res.Detail = diff
			} else {
				res.Status = StatusPass
			}
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

// defaultGoldenDir is <parent>/golden when cases live in an eval/<name> dir,
// else <cases-dir>/golden.
func defaultGoldenDir(casesDir string) string {
	parent := filepath.Dir(casesDir)
	if filepath.Base(parent) == "eval" {
		return filepath.Join(parent, "golden")
	}
	return filepath.Join(casesDir, "golden")
}

func cmdEvalSnapshot(args []string) int {
	dir, flags := popFirstArg(args)
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	golden := fs.String("golden", "", "golden 根目录(默认 <cases-dir>/../golden)")
	filter := fs.String("filter", "", "按 case 名 glob 过滤")
	record := fs.Bool("record", false, "为缺失 golden 的 case 生成 golden")
	update := fs.Bool("update", false, "刷新已有 golden")
	report := fs.String("report", "", "把 markdown 报告写到文件(缺省打印到 stdout)")
	if err := fs.Parse(flags); err != nil || fs.NArg() != 0 {
		evalUsage()
		return 2
	}
	cases, err := loadCases(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	cases = filterCases(cases, *filter)
	root := *golden
	if root == "" {
		root = defaultGoldenDir(dir)
	}
	rep := evalSnapshot(cases, root, *record, *update)
	return emitReport(rep, *report)
}

// popFirstArg splits a leading positional (the cases dir) from trailing flags,
// so Go's flag package (which stops at the first non-flag) sees only flags.
func popFirstArg(args []string) (dir string, flags []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func filterCases(cases []*Case, glob string) []*Case {
	if glob == "" {
		return cases
	}
	var out []*Case
	for _, c := range cases {
		if ok, _ := filepath.Match(glob, c.Name); ok {
			out = append(out, c)
		}
	}
	return out
}

func cmdEvalQuality(args []string) int {
	dir, flags := popFirstArg(args)
	fs := flag.NewFlagSet("quality", flag.ContinueOnError)
	filter := fs.String("filter", "", "按 case 名 glob 过滤")
	report := fs.String("report", "", "把 markdown 报告写到文件(缺省打印到 stdout)")
	if err := fs.Parse(flags); err != nil || fs.NArg() != 0 {
		evalUsage()
		return 2
	}
	cases, err := loadCases(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	cases = filterCases(cases, *filter)
	cfg, code := loadCLIConfig()
	if cfg == nil {
		return code
	}
	rep := evalQuality(cases, cfg, "")
	return emitReport(rep, *report)
}

// evalQuality runs real-strategy cases against a (real or fake) gateway. When
// childGateway is non-empty it is injected into the child; otherwise the child
// inherits the user's gateway via their real CLI config path.
func evalQuality(cases []*Case, cfg *Config, childGateway string) Report {
	rep := Report{}
	cfgPath, _ := configPath()
	for _, c := range cases {
		if c.Strategy != StrategyReal {
			continue
		}
		res := CaseResult{Name: c.Name}
		opts := RunOpts{Timeout: c.timeout()}
		if childGateway != "" {
			opts.Gateway = childGateway
		} else {
			opts.Config = cfgPath // 继承用户真实 gateway/alias,只隔离 state
		}
		out, err := runCase(c, opts)
		if err != nil {
			res.Status = StatusError
			res.Detail = err.Error()
			rep.Results = append(rep.Results, res)
			continue
		}
		res.Stdout = out.Stdout
		unmet := unmetExpectations(c, out)
		viol := trajectoryViolations(c, out.Trace)
		replay := condensedReplay(c, out.Trace)
		res.Replay = replay
		if len(unmet) > 0 {
			res.Detail = strings.Join(unmet, "\n")
		}
		if len(viol) > 0 {
			res.Detail = joinNL(res.Detail, strings.Join(viol, "\n"))
		}
		var verdict *JudgeVerdict
		var jerr error
		if c.Judge != "" {
			verdict, jerr = callJudge(cfg, c, replay)
			if jerr != nil {
				res.Detail = joinNL(res.Detail, "judge 失败: "+jerr.Error())
			}
		}
		res.Status = decideQuality(c, unmet, viol, verdict, jerr)
		if verdict != nil {
			res.Verdict = verdict
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

// emitReport writes the report markdown (file or stdout) and returns the exit code.
func emitReport(rep Report, path string) int {
	md := rep.Markdown()
	if path == "" {
		fmt.Print(md)
	} else if err := os.WriteFile(path, []byte(md), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	return rep.ExitCode()
}

func joinNL(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}

func intPtr(n int) *int { return &n }
