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
	// Name is what the host treats as the plugin's identity: it must
	// match (a) manifest.toml's `name`, (b) the install directory under
	// PluginsDir, and (c) the catalog entry in the plugin manager's
	// repo.json. The plugin manager also derives the release asset
	// name from this (`<Name>_<version>_<os>_<arch>.zip`). The
	// repository / Go module path stay `hashpoint-plugin-soggl` for
	// historical reasons; only the plugin's user-visible identity is
	// the short form.
	Name = "soggl"

	cfgEntraScope  = "entra_scope"
	cfgSogglHost   = "soggl_host"
	cfgJobsWindow  = "jobs_window"
	cfgSyncToSoggl = "sync_to_soggl"

	windowToday = "today"
	windowMonth = "month"

	defaultJobsWindow  = windowToday
	defaultSyncToSoggl = true

	nonBillablePattern = "#NF"

	// syncTimeout caps a single Hashpoint→Soggl sync pass. The host
	// invokes NotifyTagOrders fire-and-forget with its own short
	// per-call timeout; the sync runs on a detached Background context,
	// so the cap is enforced here instead.
	syncTimeout = 60 * time.Second
)

// pluginVersion is overridden at release time via GoReleaser's
// `-X github.com/.../internal/plugin.pluginVersion={{ .Version }}` ldflag,
// so the git tag is the single source of truth for the version reported
// by Metadata(). The "dev" placeholder is what regular `go build` (e.g.
// in CI's vet/test step, or a local dev build) produces.
var pluginVersion = "dev"

// config is the validated, in-memory form of sdk.PluginConfig.
type config struct {
	entraScope  string
	sogglHost   string
	jobsWindow  string
	syncToSoggl bool
}

// Plugin implements sdk.Plugin and sdk.TagProviderHandler.
type Plugin struct {
	mu     sync.RWMutex
	host   sdk.HostAPI
	cfg    config
	client *soggl.Client

	// now lets tests inject a deterministic clock.
	now func() time.Time

	// syncMu guards pendingSnapshot and syncRunning. The NotifyTagOrders
	// fire-and-forget contract requires the public method to return
	// immediately; the actual Soggl traffic runs on a single background
	// goroutine that drains pendingSnapshot until empty. Snapshots that
	// arrive while a sync is in flight overwrite pendingSnapshot rather
	// than queue up — only the latest state matters because each
	// snapshot is a complete picture of every tag.
	syncMu          sync.Mutex
	pendingSnapshot []sdk.TagOrderMapping
	syncRunning     bool

	// syncDone is closed by syncLoop after every pass when no further
	// snapshot is pending. Tests wait on it to observe completion;
	// production code never reads it. Reset to a fresh channel each
	// pass so multiple waiters can synchronise across sync cycles.
	syncDone chan struct{}
}

// New returns a Plugin ready to pass to sdk.Serve.
func New() *Plugin {
	return &Plugin{
		now:      time.Now,
		syncDone: make(chan struct{}),
	}
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
	syncToSoggl := defaultSyncToSoggl
	if raw := strings.TrimSpace(in.Fields[cfgSyncToSoggl]); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("%w: %s must be true or false", sdk.ErrConfigInvalid, cfgSyncToSoggl)
		}
		syncToSoggl = v
	}
	return config{entraScope: scope, sogglHost: host, jobsWindow: window, syncToSoggl: syncToSoggl}, nil
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

// NotifyTagOrders accepts a full Hashpoint tag-order snapshot from the
// host (delivered fire-and-forget on every user-side tag mutation) and
// schedules a Hashpoint→Soggl sync pass. The method returns immediately;
// the actual Soggl traffic runs on a single background goroutine.
//
// Always-latest semantics: while a sync is in flight, additional
// snapshots overwrite pendingSnapshot instead of queueing. The running
// goroutine drains the slot until it finds nothing pending, then exits.
// Sync is skipped entirely when cfg.syncToSoggl is false.
func (p *Plugin) NotifyTagOrders(_ context.Context, mappings []sdk.TagOrderMapping) error {
	p.mu.RLock()
	enabled := p.cfg.syncToSoggl
	configured := p.client != nil
	p.mu.RUnlock()
	if !enabled || !configured {
		return nil
	}

	p.syncMu.Lock()
	p.pendingSnapshot = mappings
	if p.syncRunning {
		p.syncMu.Unlock()
		return nil
	}
	p.syncRunning = true
	p.syncMu.Unlock()

	go p.syncLoop()
	return nil
}

// syncLoop drains pendingSnapshot. Runs on a detached goroutine; each
// pass owns its own timeout-bounded Background context so a single
// NotifyTagOrders-call ctx (which the host cancels immediately) cannot
// kill an in-progress sync.
func (p *Plugin) syncLoop() {
	for {
		p.syncMu.Lock()
		snap := p.pendingSnapshot
		if snap == nil {
			done := p.syncDone
			p.syncDone = make(chan struct{})
			p.syncRunning = false
			p.syncMu.Unlock()
			close(done)
			return
		}
		p.pendingSnapshot = nil
		p.syncMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		p.mu.RLock()
		client := p.client
		host := p.host
		p.mu.RUnlock()
		if client != nil {
			if err := p.runSync(ctx, client, host, snap); err != nil {
				logWarn(ctx, host, "soggl sync failed", err)
			}
		}
		cancel()
	}
}

// runSync executes a single Hashpoint→Soggl sync pass synchronously
// against the given client. Extracted from syncLoop so tests can drive
// the algorithm directly without goroutine timing.
func (p *Plugin) runSync(ctx context.Context, client *soggl.Client, host sdk.HostAPI, mappings []sdk.TagOrderMapping) error {
	rules, err := client.ListRules(ctx)
	if err != nil {
		return fmt.Errorf("pull soggl rules: %w", err)
	}

	// Index rules by filter, keeping the lowest-id rule per filter as
	// "the" rule for that filter. Duplicate-filter rules with higher
	// ids fall through to the disable phase below (they are not
	// directly managed by any Hashpoint tag, so they get disabled like
	// any other unowned rule).
	byFilter := make(map[string]int, len(rules))
	for i, r := range rules {
		if existing, ok := byFilter[r.Filter]; !ok || r.ID < rules[existing].ID {
			byFilter[r.Filter] = i
		}
	}

	managed := make(map[int64]struct{}, len(mappings))
	for _, m := range mappings {
		targetFilter := pathToFilter(m.TagPath)
		if targetFilter == "" {
			continue
		}
		idx, hasRule := byFilter[targetFilter]
		switch {
		case m.OrderName != "" && !hasRule:
			created, err := client.CreateRule(ctx, soggl.Rule{
				Enabled:           true,
				Score:             0,
				Filter:            targetFilter,
				NonBillable:       soggl.NonBillable{Pattern: nonBillablePattern},
				Ignore:            soggl.Ignore{},
				SoncosoAssignment: soggl.SoncosoAssignment{Fragment: m.OrderName},
			})
			if err != nil {
				logWarn(ctx, host, "soggl create rule failed", err)
				continue
			}
			managed[created.ID] = struct{}{}
		case m.OrderName != "" && hasRule:
			rule := rules[idx]
			rule.Filter = targetFilter
			rule.SoncosoAssignment.Fragment = m.OrderName
			// Re-enable: a tag with OrderName is an active mapping, so
			// flip enabled back to true even if a previous sync (or
			// the user) had disabled the rule. Without this the sync
			// would never self-heal a previously-orphaned rule whose
			// tag later regained an OrderName.
			rule.Enabled = true
			if err := client.UpdateRule(ctx, rule); err != nil {
				logWarn(ctx, host, "soggl update rule failed", err)
				continue
			}
			managed[rule.ID] = struct{}{}
		case m.OrderName == "" && hasRule:
			rule := rules[idx]
			if rule.SoncosoAssignment.Fragment == "" {
				// Already blank; nothing to do, but the rule is still
				// managed so the disable phase leaves it alone.
				managed[rule.ID] = struct{}{}
				continue
			}
			rule.SoncosoAssignment.Fragment = ""
			if err := client.UpdateRule(ctx, rule); err != nil {
				logWarn(ctx, host, "soggl blank fragment failed", err)
				continue
			}
			managed[rule.ID] = struct{}{}
		}
		// OrderName == "" && !hasRule: nothing to do.
	}

	// Disable phase: every still-enabled rule that no Hashpoint tag
	// claims gets enabled=false. Already-disabled rules are skipped
	// so the sync stays idempotent across repeated runs.
	for _, r := range rules {
		if _, ok := managed[r.ID]; ok {
			continue
		}
		if !r.Enabled {
			continue
		}
		r.Enabled = false
		if err := client.UpdateRule(ctx, r); err != nil {
			logWarn(ctx, host, "soggl disable rule failed", err)
			continue
		}
	}

	return nil
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

// pathToFilter is the inverse of filterToPath. The TagOrderMapping
// snapshot delivers paths with the leading "#" stripped from every
// segment ("lmis/betrieb"), but Soggl expects each filter token to
// carry the "#" prefix ("#lmis #betrieb"). Re-add it segment-wise and
// join with spaces.
func pathToFilter(path string) string {
	segments := strings.Split(path, "/")
	parts := make([]string, 0, len(segments))
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if !strings.HasPrefix(seg, "#") {
			seg = "#" + seg
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, " ")
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
