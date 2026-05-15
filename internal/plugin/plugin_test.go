package plugin

import (
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

// TestVersionMatchesManifest guards the manifest↔code half of the three-way
// version invariant (the third leg, tag↔manifest, is enforced in
// release.yml). Failing here means someone bumped one of plugin.Version
// or manifest.toml without bumping the other.
func TestVersionMatchesManifest(t *testing.T) {
	data, err := os.ReadFile("../../manifest.toml")
	if err != nil {
		t.Fatal(err)
	}
	verRe := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`)
	verMatch := verRe.FindStringSubmatch(string(data))
	if verMatch == nil {
		t.Fatal("could not extract version from manifest.toml")
	}
	if verMatch[1] != Version {
		t.Errorf("manifest.toml version=%q, plugin.Version=%q", verMatch[1], Version)
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
