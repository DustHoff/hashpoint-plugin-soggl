package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/dusthoff/hashpoint/plugin/sdk"

	"github.com/dusthoff/hashpoint-plugin-soggl/internal/soggl"
)

func TestFilterToPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"#solo", "#solo"},
		{"#parent #child", "#parent/#child"},
		{"#a #b #c", "#a/#b/#c"},
		{"  #spaced   #out  ", "#spaced/#out"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := filterToPath(tc.in); got != tc.want {
			t.Errorf("filterToPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPathToFilter(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"parent/child", "#parent #child"},
		{"solo", "#solo"},
		{"a/b/c", "#a #b #c"},
		{"#parent/#child", "#parent #child"}, // already-prefixed segments are not double-prefixed
		{"", ""},
		{"/", ""},
		{"  /  ", ""},
	}
	for _, tc := range cases {
		if got := pathToFilter(tc.in); got != tc.want {
			t.Errorf("pathToFilter(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJobsWindowToday(t *testing.T) {
	now := time.Date(2026, 5, 15, 14, 30, 0, 0, time.Local)
	start, end := jobsWindowFns[windowToday](now)
	want := time.Date(2026, 5, 15, 0, 0, 0, 0, time.Local)
	if !start.Equal(want) || !end.Equal(want) {
		t.Errorf("today: got (%s,%s), want (%s,%s)", start, end, want, want)
	}
}

func TestJobsWindowMonth(t *testing.T) {
	now := time.Date(2026, 5, 15, 14, 30, 0, 0, time.Local)
	start, end := jobsWindowFns[windowMonth](now)
	wantStart := time.Date(2026, 5, 15, 0, 0, 0, 0, time.Local)
	wantEnd := time.Date(2026, 6, 14, 0, 0, 0, 0, time.Local)
	if !start.Equal(wantStart) {
		t.Errorf("month start: got %s, want %s", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("month end: got %s, want %s", end, wantEnd)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(sdk.PluginConfig{Fields: map[string]string{
		cfgEntraScope: "api://soggl/.default",
		cfgSogglHost:  "https://soggl.example",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.jobsWindow != defaultJobsWindow {
		t.Errorf("jobsWindow default: got %q, want %q", cfg.jobsWindow, defaultJobsWindow)
	}
	if cfg.syncToSoggl != defaultSyncToSoggl {
		t.Errorf("syncToSoggl default: got %v, want %v", cfg.syncToSoggl, defaultSyncToSoggl)
	}
	if cfg.leafOnlySync != defaultLeafOnlySync {
		t.Errorf("leafOnlySync default: got %v, want %v", cfg.leafOnlySync, defaultLeafOnlySync)
	}
}

func TestParseConfigExplicitWindow(t *testing.T) {
	cfg, err := parseConfig(sdk.PluginConfig{Fields: map[string]string{
		cfgEntraScope: "s",
		cfgSogglHost:  "h",
		cfgJobsWindow: "month",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.jobsWindow != windowMonth {
		t.Errorf("got %q, want %q", cfg.jobsWindow, windowMonth)
	}
}

func TestParseConfigSyncToSoggl(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"True", true},
		{"1", true},
		{"0", false},
	}
	for _, tc := range cases {
		cfg, err := parseConfig(sdk.PluginConfig{Fields: map[string]string{
			cfgEntraScope:  "s",
			cfgSogglHost:   "h",
			cfgSyncToSoggl: tc.in,
		}})
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if cfg.syncToSoggl != tc.want {
			t.Errorf("%q: got %v, want %v", tc.in, cfg.syncToSoggl, tc.want)
		}
	}
}

func TestParseConfigSyncToSogglInvalid(t *testing.T) {
	_, err := parseConfig(sdk.PluginConfig{Fields: map[string]string{
		cfgEntraScope:  "s",
		cfgSogglHost:   "h",
		cfgSyncToSoggl: "yes",
	}})
	if !errors.Is(err, sdk.ErrConfigInvalid) {
		t.Errorf("got %v, want ErrConfigInvalid", err)
	}
}

func TestParseConfigLeafOnlySync(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"True", true},
		{"1", true},
		{"0", false},
	}
	for _, tc := range cases {
		cfg, err := parseConfig(sdk.PluginConfig{Fields: map[string]string{
			cfgEntraScope:   "s",
			cfgSogglHost:    "h",
			cfgLeafOnlySync: tc.in,
		}})
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if cfg.leafOnlySync != tc.want {
			t.Errorf("%q: got %v, want %v", tc.in, cfg.leafOnlySync, tc.want)
		}
	}
}

func TestParseConfigLeafOnlySyncInvalid(t *testing.T) {
	_, err := parseConfig(sdk.PluginConfig{Fields: map[string]string{
		cfgEntraScope:   "s",
		cfgSogglHost:    "h",
		cfgLeafOnlySync: "maybe",
	}})
	if !errors.Is(err, sdk.ErrConfigInvalid) {
		t.Errorf("got %v, want ErrConfigInvalid", err)
	}
}

func TestBuildParentSet(t *testing.T) {
	cases := []struct {
		name string
		in   []sdk.TagOrderMapping
		want map[string]struct{}
	}{
		{
			name: "empty",
			in:   nil,
			want: map[string]struct{}{},
		},
		{
			name: "all leaves",
			in: []sdk.TagOrderMapping{
				{TagPath: "solo"},
				{TagPath: "shared"},
			},
			want: map[string]struct{}{},
		},
		{
			name: "two-level hierarchy",
			in: []sdk.TagOrderMapping{
				{TagPath: "parent/child"},
				{TagPath: "parent/sibling"},
			},
			want: map[string]struct{}{"parent": {}},
		},
		{
			name: "parent itself in snapshot still counts as parent when descendant present",
			in: []sdk.TagOrderMapping{
				{TagPath: "parent"},
				{TagPath: "parent/child"},
			},
			want: map[string]struct{}{"parent": {}},
		},
		{
			name: "three-level adds every strict prefix",
			in: []sdk.TagOrderMapping{
				{TagPath: "a/b/c"},
			},
			want: map[string]struct{}{"a": {}, "a/b": {}},
		},
		{
			name: "empty tag path is skipped",
			in: []sdk.TagOrderMapping{
				{TagPath: ""},
				{TagPath: "x"},
			},
			want: map[string]struct{}{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildParentSet(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("size: got %d (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for k := range tc.want {
				if _, ok := got[k]; !ok {
					t.Errorf("missing parent %q in %v", k, got)
				}
			}
		})
	}
}

func TestParseConfigMissingRequired(t *testing.T) {
	cases := []sdk.PluginConfig{
		{Fields: map[string]string{}},
		{Fields: map[string]string{cfgSogglHost: "x"}},
		{Fields: map[string]string{cfgEntraScope: "x"}},
		{Fields: map[string]string{cfgEntraScope: "   ", cfgSogglHost: "x"}},
	}
	for i, c := range cases {
		if _, err := parseConfig(c); !errors.Is(err, sdk.ErrConfigInvalid) {
			t.Errorf("case %d: got %v, want ErrConfigInvalid", i, err)
		}
	}
}

func TestParseConfigBadWindow(t *testing.T) {
	_, err := parseConfig(sdk.PluginConfig{Fields: map[string]string{
		cfgEntraScope: "s",
		cfgSogglHost:  "h",
		cfgJobsWindow: "quarter",
	}})
	if !errors.Is(err, sdk.ErrConfigInvalid) {
		t.Errorf("got %v, want ErrConfigInvalid", err)
	}
}

// TestMetadata_VersionIsPlaceholder guards against accidental version
// hardcoding. The in-source `pluginVersion` MUST stay at the "dev"
// placeholder so the GoReleaser `-X` ldflag (set from the git tag) is the
// only path through which a real version string reaches Metadata().
//
// If you are tempted to change the placeholder here: don't — bump the tag
// instead and let the release workflow inject it. See the
// "Release-Pipeline & Versionierung" section in CLAUDE.md.
func TestMetadata_VersionIsPlaceholder(t *testing.T) {
	p := New()
	m, err := p.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if m.Version != "dev" {
		t.Errorf("Metadata.Version = %q, want %q (build-time injection placeholder)", m.Version, "dev")
	}
}

// TestManifestApiVersionMatchesSDK guards the manifest↔SDK half of the
// api-version invariant: a plugin compiled against sdk.HostAPIVersion=N
// must declare api_version=N in its manifest so the host's compatibility
// check accepts it. The version field is exempt — it is rewritten at
// release time by scripts/inject-manifest-version.sh.
func TestManifestApiVersionMatchesSDK(t *testing.T) {
	data, err := os.ReadFile("../../manifest.toml")
	if err != nil {
		t.Fatal(err)
	}
	apiRe := regexp.MustCompile(`(?m)^api_version\s*=\s*(\d+)`)
	apiMatch := apiRe.FindStringSubmatch(string(data))
	if apiMatch == nil {
		t.Fatal("could not extract api_version from manifest.toml")
	}
	wantAPI := strconv.Itoa(sdk.HostAPIVersion)
	if apiMatch[1] != wantAPI {
		t.Errorf("manifest.toml api_version=%s, sdk.HostAPIVersion=%s", apiMatch[1], wantAPI)
	}
}

// ---------------------------------------------------------------------
// NotifyTagOrders sync tests.
// ---------------------------------------------------------------------

// fakeSoggl is a tiny in-memory stand-in for the Soggl /api/rules
// surface. It records every PUT/POST/DELETE so a test can assert the
// exact sequence the plugin issued.
type fakeSoggl struct {
	mu       sync.Mutex
	rules    map[int64]soggl.Rule
	nextID   int64
	puts     []soggl.Rule
	posts    []soggl.Rule
	getCount int
}

func newFakeSoggl(initial ...soggl.Rule) *fakeSoggl {
	f := &fakeSoggl{rules: make(map[int64]soggl.Rule)}
	for _, r := range initial {
		f.rules[r.ID] = r
		if r.ID >= f.nextID {
			f.nextID = r.ID
		}
	}
	return f
}

func (f *fakeSoggl) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/rules":
			f.mu.Lock()
			f.getCount++
			rules := make([]soggl.Rule, 0, len(f.rules))
			for _, rl := range f.rules {
				rules = append(rules, rl)
			}
			f.mu.Unlock()
			// Deterministic order helps tests assert without sorting.
			sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
			_ = json.NewEncoder(w).Encode(rules)
		case r.Method == http.MethodPost && r.URL.Path == "/api/rules":
			var body soggl.Rule
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.nextID++
			body.ID = f.nextID
			f.rules[body.ID] = body
			f.posts = append(f.posts, body)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPut && len(r.URL.Path) > len("/api/rules/") && r.URL.Path[:len("/api/rules/")] == "/api/rules/":
			id, err := strconv.ParseInt(r.URL.Path[len("/api/rules/"):], 10, 64)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var body soggl.Rule
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body.ID = id
			f.mu.Lock()
			f.rules[id] = body
			f.puts = append(f.puts, body)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

// nopHost only implements the two HostAPI methods the plugin actually
// calls (RequestEntraToken, Log). Every other method panics, which
// surfaces an accidental call in tests rather than silently masking it.
type nopHost struct{ sdk.HostAPI }

func (h nopHost) RequestEntraToken(_ context.Context, _ []string) (string, time.Time, error) {
	return "tok", time.Now().Add(time.Hour), nil
}

func (h nopHost) Log(_ context.Context, _, _ string, _ map[string]string) error { return nil }

// configuredPlugin wires a Plugin against a fakeSoggl-backed httptest
// server. Returned cleanup tears down the server.
func configuredPlugin(t *testing.T, fake *fakeSoggl, syncToSoggl bool) (*Plugin, func()) {
	t.Helper()
	srv := httptest.NewServer(fake.handler(t))
	p := New()
	if err := p.Init(context.Background(), nopHost{}); err != nil {
		srv.Close()
		t.Fatalf("Init: %v", err)
	}
	syncVal := "false"
	if syncToSoggl {
		syncVal = "true"
	}
	cfg := sdk.PluginConfig{Fields: map[string]string{
		cfgEntraScope:  "scope",
		cfgSogglHost:   srv.URL,
		cfgSyncToSoggl: syncVal,
	}}
	if err := p.Configure(context.Background(), cfg); err != nil {
		srv.Close()
		t.Fatalf("Configure: %v", err)
	}
	return p, srv.Close
}

func TestRunSync_CreatesMissingRule(t *testing.T) {
	fake := newFakeSoggl()
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "parent/child", OrderName: "Order Alpha 2026"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.posts) != 1 {
		t.Fatalf("posts: got %d, want 1", len(fake.posts))
	}
	got := fake.posts[0]
	if got.Filter != "#parent #child" {
		t.Errorf("filter: got %q, want %q", got.Filter, "#parent #child")
	}
	if got.SoncosoAssignment.Fragment != "Order Alpha 2026" {
		t.Errorf("fragment: got %q", got.SoncosoAssignment.Fragment)
	}
	if !got.Enabled {
		t.Errorf("new rule should be enabled")
	}
	if got.NonBillable.Pattern != nonBillablePattern {
		t.Errorf("nonBillable.pattern: got %q, want %q", got.NonBillable.Pattern, nonBillablePattern)
	}
}

func TestRunSync_UpdatesExistingPreservesFields(t *testing.T) {
	existing := soggl.Rule{
		ID:                42,
		Enabled:           true,
		Score:             214, // user-tuned, must survive
		Filter:            "#parent #child",
		NonBillable:       soggl.NonBillable{Pattern: "#NF", DefaultToNonBillable: true},
		Ignore:            soggl.Ignore{},
		SoncosoAssignment: soggl.SoncosoAssignment{Fragment: "old fragment"},
	}
	fake := newFakeSoggl(existing)
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "parent/child", OrderName: "new fragment"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 1 {
		t.Fatalf("puts: got %d, want 1", len(fake.puts))
	}
	got := fake.puts[0]
	if got.ID != 42 {
		t.Errorf("id: got %d, want 42", got.ID)
	}
	if got.SoncosoAssignment.Fragment != "new fragment" {
		t.Errorf("fragment: got %q", got.SoncosoAssignment.Fragment)
	}
	if got.Score != 214 {
		t.Errorf("score not preserved: got %d, want 214", got.Score)
	}
	if !got.NonBillable.DefaultToNonBillable {
		t.Errorf("nonBillable.defaultToNonBillable not preserved")
	}
	if !got.Enabled {
		t.Errorf("enabled flipped unexpectedly")
	}
}

func TestRunSync_DuplicateFilterLowestIdWins(t *testing.T) {
	// Two rules share "#shared" — only id=5 (lowest) gets the fragment
	// update; id=9 falls through to disable phase.
	fake := newFakeSoggl(
		soggl.Rule{ID: 5, Enabled: true, Filter: "#shared", SoncosoAssignment: soggl.SoncosoAssignment{Fragment: "old A"}},
		soggl.Rule{ID: 9, Enabled: true, Filter: "#shared", SoncosoAssignment: soggl.SoncosoAssignment{Fragment: "old B"}},
	)
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "shared", OrderName: "Shared Order"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	puts := fake.puts
	// Expect exactly two PUTs: id=5 update + id=9 disable. Order is
	// deterministic in this test (managed-loop runs first, disable
	// phase iterates rules by their pulled order which we sorted by ID).
	if len(puts) != 2 {
		t.Fatalf("puts: got %d, want 2 (id=5 update + id=9 disable)", len(puts))
	}
	var updated, disabled *soggl.Rule
	for i := range puts {
		switch puts[i].ID {
		case 5:
			updated = &puts[i]
		case 9:
			disabled = &puts[i]
		}
	}
	if updated == nil {
		t.Fatal("no PUT for id=5")
	}
	if updated.SoncosoAssignment.Fragment != "Shared Order" {
		t.Errorf("id=5 fragment: got %q, want %q", updated.SoncosoAssignment.Fragment, "Shared Order")
	}
	if !updated.Enabled {
		t.Errorf("id=5 should stay enabled")
	}
	if disabled == nil {
		t.Fatal("no PUT for id=9 (disable)")
	}
	if disabled.Enabled {
		t.Errorf("id=9 should be disabled")
	}
}

func TestRunSync_DisablesOrphans(t *testing.T) {
	fake := newFakeSoggl(
		soggl.Rule{ID: 1, Enabled: true, Filter: "#dead", SoncosoAssignment: soggl.SoncosoAssignment{Fragment: "Old"}},
		soggl.Rule{ID: 2, Enabled: false, Filter: "#already-off", SoncosoAssignment: soggl.SoncosoAssignment{Fragment: "Old"}},
	)
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	// id=1 disabled, id=2 already disabled so untouched.
	if len(fake.puts) != 1 {
		t.Fatalf("puts: got %d, want 1", len(fake.puts))
	}
	if fake.puts[0].ID != 1 || fake.puts[0].Enabled {
		t.Errorf("unexpected disable PUT: %+v", fake.puts[0])
	}
}

func TestRunSync_ReEnablesPreviouslyDisabledRule(t *testing.T) {
	// A rule that an earlier sync (or the user) disabled regains an
	// OrderName: the rule must come back to enabled=true so the sync
	// is self-healing.
	fake := newFakeSoggl(soggl.Rule{
		ID:                11,
		Enabled:           false,
		Filter:            "#proj",
		SoncosoAssignment: soggl.SoncosoAssignment{Fragment: ""},
	})
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "proj", OrderName: "Reactivated"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 1 {
		t.Fatalf("puts: got %d, want 1", len(fake.puts))
	}
	got := fake.puts[0]
	if !got.Enabled {
		t.Errorf("expected enabled=true on re-mapped rule")
	}
	if got.SoncosoAssignment.Fragment != "Reactivated" {
		t.Errorf("fragment: got %q", got.SoncosoAssignment.Fragment)
	}
}

func TestRunSync_BlankFragmentOnOrderRemoval(t *testing.T) {
	fake := newFakeSoggl(soggl.Rule{
		ID:                7,
		Enabled:           true,
		Filter:            "#proj",
		SoncosoAssignment: soggl.SoncosoAssignment{Fragment: "Was here"},
	})
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "proj", OrderName: ""},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 1 {
		t.Fatalf("puts: got %d, want 1", len(fake.puts))
	}
	got := fake.puts[0]
	if got.SoncosoAssignment.Fragment != "" {
		t.Errorf("fragment: got %q, want empty", got.SoncosoAssignment.Fragment)
	}
	if !got.Enabled {
		t.Errorf("blanking fragment should leave enabled=true")
	}
}

func TestRunSync_BlankAlreadyBlankIsNoop(t *testing.T) {
	// Tag with no OrderName + rule already has empty fragment ⇒ rule
	// is "managed" (so disable phase leaves it alone) but no PUT is
	// issued. Keeps sync idempotent.
	fake := newFakeSoggl(soggl.Rule{
		ID: 7, Enabled: true, Filter: "#proj",
		SoncosoAssignment: soggl.SoncosoAssignment{Fragment: ""},
	})
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "proj", OrderName: ""},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 0 {
		t.Errorf("expected no PUTs (rule already in desired state), got %d", len(fake.puts))
	}
}

func TestRunSync_LeafOnly_SkipsParentAndDisablesItsRule(t *testing.T) {
	// Snapshot has a parent + two children. With leafOnly=true the
	// parent is skipped (no Create/Update/Disable-as-managed) — the
	// pre-existing rule for #parent falls into the disable phase like
	// any other unowned rule. The two leaf rules are POSTed.
	existingParent := soggl.Rule{
		ID: 1, Enabled: true, Filter: "#parent",
		SoncosoAssignment: soggl.SoncosoAssignment{Fragment: "Parent fragment"},
	}
	fake := newFakeSoggl(existingParent)
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "parent", OrderName: "Should be ignored"},
		{TagPath: "parent/child", OrderName: "Order A"},
		{TagPath: "parent/sibling", OrderName: "Order B"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	// Two POSTs (the two leaves); the parent must not appear.
	if len(fake.posts) != 2 {
		t.Fatalf("posts: got %d, want 2", len(fake.posts))
	}
	for _, p := range fake.posts {
		if p.Filter == "#parent" {
			t.Errorf("parent rule was created even though leafOnly is true: %+v", p)
		}
	}

	// One PUT: the pre-existing parent rule disabled by the orphan phase.
	if len(fake.puts) != 1 {
		t.Fatalf("puts: got %d, want 1 (parent-rule disable)", len(fake.puts))
	}
	got := fake.puts[0]
	if got.ID != 1 || got.Enabled {
		t.Errorf("expected id=1 disabled, got %+v", got)
	}
}

func TestRunSync_LeafOnly_ParentWithOrderNameStillSkipped(t *testing.T) {
	// Even if the parent has its own OrderName, leafOnly=true means
	// "strict skip" — the parent is treated the same as if it had no
	// order. No rule is created for it; an existing one would be
	// disabled (covered by the previous test).
	fake := newFakeSoggl()
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "parent", OrderName: "Parent order"},
		{TagPath: "parent/child", OrderName: "Leaf order"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	if len(fake.posts) != 1 {
		t.Fatalf("posts: got %d, want 1", len(fake.posts))
	}
	if fake.posts[0].Filter != "#parent #child" {
		t.Errorf("expected only the leaf to be created, got %+v", fake.posts[0])
	}
}

func TestRunSync_LeafOnlyDisabled_SyncsParentsToo(t *testing.T) {
	// leafOnly=false restores the pre-feature behaviour: every tag with
	// an OrderName is synced, parents included.
	fake := newFakeSoggl()
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	err := p.runSync(context.Background(), p.client, p.host, []sdk.TagOrderMapping{
		{TagPath: "parent", OrderName: "Parent order"},
		{TagPath: "parent/child", OrderName: "Leaf order"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.posts) != 2 {
		t.Fatalf("posts: got %d, want 2 (parent + leaf)", len(fake.posts))
	}
	filters := map[string]bool{}
	for _, p := range fake.posts {
		filters[p.Filter] = true
	}
	if !filters["#parent"] || !filters["#parent #child"] {
		t.Errorf("missing expected filters in %v", filters)
	}
}

func TestNotifyTagOrders_KillSwitch(t *testing.T) {
	// sync_to_soggl=false ⇒ NotifyTagOrders returns nil without
	// touching the Soggl API at all.
	fake := newFakeSoggl(soggl.Rule{ID: 1, Enabled: true, Filter: "#orphan"})
	p, cleanup := configuredPlugin(t, fake, false)
	defer cleanup()

	err := p.NotifyTagOrders(context.Background(), []sdk.TagOrderMapping{
		{TagPath: "parent/child", OrderName: "X"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Give any rogue goroutine a moment to misbehave; expect nothing.
	time.Sleep(50 * time.Millisecond)
	if fake.getCount != 0 || len(fake.puts) != 0 || len(fake.posts) != 0 {
		t.Errorf("kill-switch off but Soggl was touched: get=%d puts=%d posts=%d",
			fake.getCount, len(fake.puts), len(fake.posts))
	}
}

func TestNotifyTagOrders_AsyncCompletes(t *testing.T) {
	// Full integration through the async wrapper: NotifyTagOrders
	// returns immediately, the background loop pulls + writes, and
	// closing of the syncDone channel signals completion.
	fake := newFakeSoggl()
	p, cleanup := configuredPlugin(t, fake, true)
	defer cleanup()

	p.syncMu.Lock()
	done := p.syncDone
	p.syncMu.Unlock()

	if err := p.NotifyTagOrders(context.Background(), []sdk.TagOrderMapping{
		{TagPath: "parent/child", OrderName: "Order A"},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("syncDone not closed within 2s")
	}
	if len(fake.posts) != 1 {
		t.Errorf("expected 1 POST, got %d", len(fake.posts))
	}
}

func TestNotifyTagOrders_AlwaysLatestCoalesces(t *testing.T) {
	// A slow Soggl GET lets us pile up several NotifyTagOrders calls
	// before the first sync finishes. The always-latest contract says
	// only the most-recent snapshot's POSTs should land — earlier
	// snapshots get overwritten.
	release := make(chan struct{})
	var pulledOnce atomic.Bool

	fake := newFakeSoggl()
	// Wrap the fake handler so the first GET blocks until release closes.
	mux := http.NewServeMux()
	inner := fake.handler(t)
	mux.HandleFunc("/api/rules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && !pulledOnce.Swap(true) {
			<-release
		}
		inner.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/rules/", inner.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New()
	if err := p.Init(context.Background(), nopHost{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(context.Background(), sdk.PluginConfig{Fields: map[string]string{
		cfgEntraScope:  "scope",
		cfgSogglHost:   srv.URL,
		cfgSyncToSoggl: "true",
	}}); err != nil {
		t.Fatal(err)
	}

	// First call kicks off the sync that immediately blocks on the GET.
	if err := p.NotifyTagOrders(context.Background(), []sdk.TagOrderMapping{
		{TagPath: "first", OrderName: "FIRST"},
	}); err != nil {
		t.Fatal(err)
	}
	// Pile up two more snapshots while the first sync is stuck.
	if err := p.NotifyTagOrders(context.Background(), []sdk.TagOrderMapping{
		{TagPath: "middle", OrderName: "MIDDLE"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.NotifyTagOrders(context.Background(), []sdk.TagOrderMapping{
		{TagPath: "last", OrderName: "LAST"},
	}); err != nil {
		t.Fatal(err)
	}

	// Release the GET. The first sync processes the FIRST snapshot;
	// the loop then picks up LAST (MIDDLE was overwritten in the slot
	// before the loop could observe it).
	close(release)

	// Wait for the loop to drain by polling syncRunning.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.syncMu.Lock()
		running := p.syncRunning
		p.syncMu.Unlock()
		if !running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.syncMu.Lock()
	if p.syncRunning {
		p.syncMu.Unlock()
		t.Fatal("sync loop still running after 3s")
	}
	p.syncMu.Unlock()

	// We expect at most two POSTs: FIRST + LAST. MIDDLE must have been
	// dropped in the pendingSnapshot slot before the loop picked it up.
	fake.mu.Lock()
	posts := append([]soggl.Rule(nil), fake.posts...)
	fake.mu.Unlock()
	fragments := make(map[string]bool, len(posts))
	for _, p := range posts {
		fragments[p.SoncosoAssignment.Fragment] = true
	}
	if fragments["MIDDLE"] {
		t.Errorf("MIDDLE snapshot leaked through — always-latest contract broken")
	}
	if !fragments["LAST"] {
		t.Errorf("LAST snapshot missing — loop did not drain pendingSnapshot")
	}
}
