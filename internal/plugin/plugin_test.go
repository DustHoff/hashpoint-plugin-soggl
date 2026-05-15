package plugin

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	sdk "github.com/dusthoff/hashpoint/plugin/sdk"
)

func TestFilterToPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"#deg", "#deg"},
		{"#lmis #betrieb", "#lmis/#betrieb"},
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
		cfgSogglHost:  "https://soggl.lmis.de",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.jobsWindow != defaultJobsWindow {
		t.Errorf("jobsWindow default: got %q, want %q", cfg.jobsWindow, defaultJobsWindow)
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
