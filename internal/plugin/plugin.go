// Package plugin wires the Soggl client into the Hashpoint plugin SDK.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/dusthoff/hashpoint/plugin/sdk"

	"github.com/dusthoff/hashpoint-plugin-soggl/internal/soggl"
)

const (
	Name = "hashpoint-plugin-soggl"

	cfgEntraScope = "entra_scope"
	cfgSogglHost  = "soggl_host"
	cfgJobsWindow = "jobs_window"

	windowToday = "today"
	windowMonth = "month"

	defaultJobsWindow = windowToday
)

// pluginVersion is overridden at release time via GoReleaser's
// `-X github.com/.../internal/plugin.pluginVersion={{ .Version }}` ldflag,
// so the git tag is the single source of truth for the version reported
// by Metadata(). The "dev" placeholder is what regular `go build` (e.g.
// in CI's vet/test step, or a local dev build) produces.
var pluginVersion = "dev"

// config is the validated, in-memory form of sdk.PluginConfig.
type config struct {
	entraScope string
	sogglHost  string
	jobsWindow string
}

// Plugin implements sdk.Plugin and sdk.TagProviderHandler.
type Plugin struct {
	mu     sync.RWMutex
	host   sdk.HostAPI
	cfg    config
	client *soggl.Client

	// now lets tests inject a deterministic clock.
	now func() time.Time
}

// New returns a Plugin ready to pass to sdk.Serve.
func New() *Plugin {
	return &Plugin{now: time.Now}
}

func (p *Plugin) Init(_ context.Context, host sdk.HostAPI) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = host
	return nil
}

func (p *Plugin) Metadata(_ context.Context) (sdk.Metadata, error) {
	return sdk.Metadata{
		Name:         Name,
		Version:      pluginVersion,
		APIVersion:   sdk.HostAPIVersion,
		Capabilities: []sdk.Capability{sdk.CapTagProvider},
		Description:  "Tag provider bridging Hashpoint with the internal Soggl application.",
	}, nil
}

func (p *Plugin) Configure(_ context.Context, in sdk.PluginConfig) error {
	cfg, err := parseConfig(in)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == nil {
		return errors.New("plugin: Configure called before Init")
	}
	p.cfg = cfg
	p.client = soggl.NewClient(cfg.sogglHost, tokenFunc(p.host, cfg.entraScope))
	return nil
}

func parseConfig(in sdk.PluginConfig) (config, error) {
	scope := strings.TrimSpace(in.Fields[cfgEntraScope])
	if scope == "" {
		return config{}, fmt.Errorf("%w: %s is required", sdk.ErrConfigInvalid, cfgEntraScope)
	}
	host := strings.TrimSpace(in.Fields[cfgSogglHost])
	if host == "" {
		return config{}, fmt.Errorf("%w: %s is required", sdk.ErrConfigInvalid, cfgSogglHost)
	}
	window := strings.TrimSpace(in.Fields[cfgJobsWindow])
	if window == "" {
		window = defaultJobsWindow
	}
	if _, ok := jobsWindowFns[window]; !ok {
		return config{}, fmt.Errorf("%w: %s must be one of: %s, %s", sdk.ErrConfigInvalid, cfgJobsWindow, windowToday, windowMonth)
	}
	return config{entraScope: scope, sogglHost: host, jobsWindow: window}, nil
}

// jobsWindowFns maps a jobs_window value to a function computing
// (start, end) from a reference now. Adding a new window value is a
// one-line addition here.
var jobsWindowFns = map[string]func(now time.Time) (time.Time, time.Time){
	windowToday: func(now time.Time) (time.Time, time.Time) {
		d := dateOnly(now)
		return d, d
	},
	windowMonth: func(now time.Time) (time.Time, time.Time) {
		d := dateOnly(now)
		return d, d.AddDate(0, 0, 30)
	},
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func (p *Plugin) ListTags(ctx context.Context) ([]sdk.ImportedTag, error) {
	p.mu.RLock()
	client := p.client
	host := p.host
	p.mu.RUnlock()
	if client == nil {
		return nil, sdk.ErrNotConfigured
	}

	rules, err := client.ListRules(ctx)
	if err != nil {
		logWarn(ctx, host, "soggl list rules failed", err)
		return nil, err
	}

	out := make([]sdk.ImportedTag, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		path := filterToPath(r.Filter)
		if path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, sdk.ImportedTag{Path: path})
	}
	return out, nil
}

func (p *Plugin) ListOrders(ctx context.Context) ([]sdk.Order, error) {
	p.mu.RLock()
	client := p.client
	host := p.host
	window := p.cfg.jobsWindow
	now := p.now
	p.mu.RUnlock()
	if client == nil {
		return nil, sdk.ErrNotConfigured
	}
	fn, ok := jobsWindowFns[window]
	if !ok {
		return nil, fmt.Errorf("%w: %s", sdk.ErrConfigInvalid, cfgJobsWindow)
	}
	start, end := fn(now())

	jobs, err := client.ListJobs(ctx, start, end)
	if err != nil {
		logWarn(ctx, host, "soggl list jobs failed", err)
		return nil, err
	}

	out := make([]sdk.Order, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, sdk.Order{
			ID:          strconv.FormatInt(j.ID, 10),
			Name:        j.Label,
			Description: j.Customer,
		})
	}
	return out, nil
}

// filterToPath converts a Soggl rule filter ("#parent #child") into a
// slash-separated Hashpoint tag path ("#parent/#child"). A single
// "#tag" yields a top-level tag. The host's EnsureByPath normalises
// each segment (strips '#', drops non-alphanumeric, re-prefixes '#'),
// so segments are forwarded unchanged.
func filterToPath(filter string) string {
	parts := strings.Fields(filter)
	return strings.Join(parts, "/")
}

func tokenFunc(host sdk.HostAPI, scope string) soggl.TokenFunc {
	return func(ctx context.Context) (string, error) {
		token, _, err := host.RequestEntraToken(ctx, []string{scope})
		return token, err
	}
}

func logWarn(ctx context.Context, host sdk.HostAPI, message string, err error) {
	if host == nil {
		return
	}
	_ = host.Log(ctx, "warn", message, map[string]string{"err": err.Error()})
}
