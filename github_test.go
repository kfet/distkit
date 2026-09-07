package distkit

import (
	"fmt"
	"strings"
	"testing"
)

func TestDiscoverToken(t *testing.T) {
	restore := tokenCommand
	t.Cleanup(func() { tokenCommand = restore })

	t.Run("GITHUB_TOKEN wins", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "from-github-token")
		t.Setenv("GH_TOKEN", "from-gh-token")
		tokenCommand = func() ([]byte, error) { return []byte("from-gh"), nil }
		if got := DiscoverToken(); got != "from-github-token" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("GH_TOKEN is the fallback", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "from-gh-token")
		tokenCommand = func() ([]byte, error) { return []byte("from-gh"), nil }
		if got := DiscoverToken(); got != "from-gh-token" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("gh auth token last", func(t *testing.T) {
		// A fleet host usually has gh configured but exports no token, so
		// this branch is the one that actually fires in production.
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		tokenCommand = func() ([]byte, error) { return []byte("  gho_fromgh\n"), nil }
		if got := DiscoverToken(); got != "gho_fromgh" {
			t.Fatalf("got %q — the trailing newline must be trimmed", got)
		}
	})

	t.Run("no token anywhere is not an error", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		tokenCommand = func() ([]byte, error) { return nil, fmt.Errorf("gh: not logged in") }
		if got := DiscoverToken(); got != "" {
			t.Fatalf("got %q — an anonymous run is legitimate against a public repo", got)
		}
	})
}

func TestLookupChecksum(t *testing.T) {
	manifest := []byte(strings.Join([]string{
		"aaa111  zulip-acp-linux-amd64",
		"bbb222  zulip-acp-linux-arm64",
		"ccc333 *zulip-acp-darwin-arm64", // binary-mode marker
		"",
		"   ",
		"malformed-line-without-a-name",
	}, "\n"))

	for name, want := range map[string]string{
		"zulip-acp-linux-amd64":  "aaa111",
		"zulip-acp-linux-arm64":  "bbb222",
		"zulip-acp-darwin-arm64": "ccc333",
	} {
		got, err := LookupChecksum(manifest, name)
		if err != nil || got != want {
			t.Errorf("LookupChecksum(%s) = (%q, %v), want %q", name, got, err, want)
		}
	}

	if _, err := LookupChecksum(manifest, "zulip-acp-linux-arm"); err == nil {
		t.Error("a name that is a prefix of another must not match")
	}
	if _, err := LookupChecksum(nil, "anything"); err == nil {
		t.Error("want an error for an empty manifest")
	}
}

func TestReleaseAssetURL(t *testing.T) {
	rel := &Release{TagName: "v1.0.0"}
	rel.Assets = append(rel.Assets, struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{Name: "tool-linux-amd64", URL: "https://api.github.com/repos/k/t/releases/assets/1"})

	got, err := rel.AssetURL("tool-linux-amd64")
	if err != nil || !strings.HasSuffix(got, "/assets/1") {
		t.Fatalf("got (%q, %v)", got, err)
	}
	if _, err := rel.AssetURL("nope"); err == nil || !strings.Contains(err.Error(), "v1.0.0") {
		t.Fatalf("the error must name the release: %v", err)
	}
}

func TestFetchReleaseValidatesConfig(t *testing.T) {
	if _, err := FetchRelease(t.Context(), Config{}, ""); err == nil {
		t.Fatal("want a validation error")
	}
}
