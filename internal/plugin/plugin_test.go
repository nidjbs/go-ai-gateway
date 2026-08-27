package plugin

import (
	"errors"
	"testing"

	"github.com/nidjbs/go-ai-gateway/internal/provider"
)

// testPlugin is a controllable before_request plugin.
type testPlugin struct {
	name   string
	typ    PluginType
	before func(ctx *Context) error
	after  func(ctx *Context) error
}

func (p *testPlugin) Name() string                     { return p.name }
func (p *testPlugin) Type() PluginType                 { return p.typ }
func (p *testPlugin) BeforeRequest(ctx *Context) error { return p.before(ctx) }
func (p *testPlugin) AfterRequest(ctx *Context) error  { return p.after(ctx) }

func TestManagerRunsBeforeInOrder(t *testing.T) {
	var order []string
	m := NewManager(nil)
	m.Add(&testPlugin{name: "a", typ: PluginTypeGuardrail, before: func(ctx *Context) error {
		order = append(order, "a")
		return nil
	}})
	m.Add(&testPlugin{name: "b", typ: PluginTypeGuardrail, before: func(ctx *Context) error {
		order = append(order, "b")
		return nil
	}})
	if err := m.RunBefore(&Context{}); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order = %v, want [a b]", order)
	}
}

func TestManagerRejectionStopsChain(t *testing.T) {
	var ranB bool
	m := NewManager(nil)
	m.Add(&testPlugin{name: "guard", typ: PluginTypeGuardrail, before: func(ctx *Context) error {
		return &RejectionError{StatusCode: 429, Code: "prompt_injection_detected", Reason: "blocked"}
	}})
	m.Add(&testPlugin{name: "b", typ: PluginTypeGuardrail, before: func(ctx *Context) error {
		ranB = true
		return nil
	}})
	err := m.RunBefore(&Context{})
	re, ok := AsRejection(err)
	if !ok {
		t.Fatalf("err = %v, want RejectionError", err)
	}
	if re.Status() != 429 || re.Code != "prompt_injection_detected" {
		t.Errorf("rejection = %+v, want 429/prompt_injection_detected", re)
	}
	if ranB {
		t.Error("chain continued after rejection")
	}
}

func TestManagerFailClosedByDefault(t *testing.T) {
	m := NewManager(nil)
	m.Add(&testPlugin{name: "guard", typ: PluginTypeGuardrail, before: func(ctx *Context) error {
		return errors.New("scanner down")
	}})
	err := m.RunBefore(&Context{})
	var fe *FailureError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want FailureError", err)
	}
	if fe.Stage != StageBeforeRequest || fe.PluginType != PluginTypeGuardrail {
		t.Errorf("failure = %+v, want guardrail before_request", fe)
	}
}

func TestManagerObservabilityFailsOpen(t *testing.T) {
	m := NewManager(nil)
	m.Add(&testPlugin{name: "log", typ: PluginTypeLogging, before: func(ctx *Context) error {
		return errors.New("sink down")
	}})
	if err := m.RunBefore(&Context{}); err != nil {
		t.Fatalf("logging plugin must fail open, got %v", err)
	}
}

func TestManagerMultiStagePlugin(t *testing.T) {
	seen := map[Stage]bool{}
	both := &testPlugin{name: "both", typ: PluginTypeTransform}
	both.before = func(ctx *Context) error { seen[StageBeforeRequest] = true; return nil }
	both.after = func(ctx *Context) error { seen[StageAfterRequest] = true; return nil }
	m := NewManager(nil)
	m.Add(both)
	if err := m.RunBefore(&Context{}); err != nil {
		t.Fatal(err)
	}
	if err := m.RunAfter(&Context{}); err != nil {
		t.Fatal(err)
	}
	if !seen[StageBeforeRequest] || !seen[StageAfterRequest] {
		t.Fatalf("stages seen = %v, want both", seen)
	}
}

func TestManagerContextValuesPropagate(t *testing.T) {
	m := NewManager(nil)
	m.Add(&testPlugin{name: "writer", typ: PluginTypeTransform, before: func(ctx *Context) error {
		ctx.SetValue("scan", 0.9)
		return nil
	}})
	m.Add(&testPlugin{name: "reader", typ: PluginTypeGuardrail, before: func(ctx *Context) error {
		if got := ctx.Value("scan"); got != 0.9 {
			t.Fatalf("reader saw %v, want 0.9", got)
		}
		return nil
	}})
	if err := m.RunBefore(&Context{}); err != nil {
		t.Fatal(err)
	}
}

func TestOnErrorReceivesErr(t *testing.T) {
	want := errors.New("boom")
	var got error
	m := NewManager(nil)
	m.Add(&onErrorPlugin{fn: func(ctx *Context) error { got = ctx.Err; return nil }})
	ctx := &Context{Err: want}
	if err := m.RunOnError(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(got, want) {
		t.Fatalf("onError got %v, want %v", got, want)
	}
}

type onErrorPlugin struct {
	fn func(ctx *Context) error
}

func (p *onErrorPlugin) Name() string               { return "onerr" }
func (p *onErrorPlugin) Type() PluginType           { return PluginTypeLogging }
func (p *onErrorPlugin) OnError(ctx *Context) error { return p.fn(ctx) }

func TestManagerNilPluginIgnored(t *testing.T) {
	m := NewManager(nil)
	m.Add(nil)
	if m.Len() != 0 {
		t.Fatalf("Len = %d, want 0", m.Len())
	}
}

func TestUsageCarriedToAfter(t *testing.T) {
	want := provider.Usage{InputTokens: 5, OutputTokens: 7}
	var got provider.Usage
	m := NewManager(nil)
	m.Add(&testPlugin{name: "a", typ: PluginTypeMetrics, after: func(ctx *Context) error {
		got = ctx.Usage
		return nil
	}})
	if err := m.RunAfter(&Context{Usage: want}); err != nil {
		t.Fatal(err)
	}
	if got.InputTokens != 5 || got.OutputTokens != 7 {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestRegistryBuild(t *testing.T) {
	Register("test-x", func(opts map[string]any) (Plugin, error) {
		return &testPlugin{name: "test-x", typ: PluginTypeGuardrail}, nil
	})
	p, err := Build("test-x", nil)
	if err != nil || p.Name() != "test-x" {
		t.Fatalf("Build = %v, %v", p, err)
	}
	if _, err := Build("nope", nil); err == nil {
		t.Fatal("expected error for unregistered name")
	}
}
