# distkit

One Go module that owns **binary distribution and self-update** for the kfet
family of tools — `fir`, `harb`, `mintick`, and the ACP relays.

It replaces four hand-written self-update implementations (each missing a
different piece) and six divergent `install.sh` scripts.

- **No dependency on ACP, on any relay, or on fir.** Every tool in the family
  must be able to import it, so it depends on nothing but the standard
  library.
- Go 1.25.

```
go get github.com/kfet/distkit
```

## Self-update in one call

```go
import "github.com/kfet/distkit"

func main() {
    if len(os.Args) > 1 && os.Args[1] == "update" {
        os.Exit(distkit.Main(distkit.Config{
            Repo:        "kfet/zulip-acp",              // GitHub slug
            Binary:      "zulip-acp",                   // installed binary name
            AssetStem:   "zulip-acp",                   // asset is <stem>-<os>-<arch>
            Version:     version,                       // compiled-in current version
            RestartHint: "systemctl --user reload zulip-acp",
        }))
    }
    ...
}
```

That gives the tool a complete `update` subcommand:

```
zulip-acp update                       # install the latest release
zulip-acp update -check                # report only, install nothing
zulip-acp update -version v0.19.0      # install a specific release
zulip-acp update -repo kfet/other      # take releases from elsewhere
zulip-acp update -restart-cmd 'systemctl --user reload zulip-acp'
```

Exit codes: `0` success (including *already up to date*), `1` the update
failed or was refused, `2` the flags did not parse, **`3`** a `-check` run
found a newer release — so a timer or cron nag can act on the result without
parsing stdout.

## What it does, and why

**Everything goes through the GitHub REST API — asset bytes included.** The
release is resolved with `Accept: application/vnd.github+json`, the asset is
fetched from its API URL with `Accept: application/octet-stream`, and both
carry a bearer token when one can be found. This is the only shape that works
against a repo that is still private, and it works unauthenticated against a
public one, so there is one code path rather than two. The token is taken from
`GITHUB_TOKEN`, then `GH_TOKEN`, then `gh auth token` — a fleet host usually
has `gh` configured but exports no token. It is worth having even against a
public repo: GitHub's unauthenticated rate limit is per **IP address**, so a
fleet behind one NAT exhausts it between them, and the resulting 403 reads
like a permissions failure. A missing-token 404 and a spent-limit 403 are
reported as the different problems they are.

**The download is verified before anything moves.** The asset's sha256 is
computed inline as it streams to disk (never read back) and compared with the
release's `checksums.txt`.

**The swap is ETXTBSY-safe and atomic.** The new binary is staged in a temp
directory *next to* the running one — same directory, therefore same
filesystem — and `os.Rename`d over it. A running executable cannot be written
or truncated in place, but its directory entry can be replaced; the live
process keeps its old inode mapped until it re-execs. That is why a swapped
binary is inert until you recycle the service, and why `RestartHint` exists.

**A Homebrew install is upgraded, not refused.** The executable is resolved
through symlinks; a `Cellar` component under one of the three standard
prefixes (`/opt/homebrew`, `/usr/local`, `/home/linuxbrew/.linuxbrew`) — or
under whatever `brew --prefix` reports — is a keg. The fully-qualified formula
(`kfet/ai/fir`) comes from the keg's `INSTALL_RECEIPT.json`, falling back to
`brew info --json=v2`, so `brew upgrade` cannot bind to a same-named formula
from another tap. Then `brew update && brew upgrade <formula>` runs with no
timeout (it may compile) and its output streamed through. Self-updating a keg
would be silently reverted by the next `brew upgrade`, leaving a host that
reports one version and runs another.

Detection is deliberately conservative: every uncertain case reports *not
brew*, because a false positive breaks `update` on a non-brew host while a
false negative only falls back to the normal path. The one case that is
refused outright is a confirmed keg with `brew` absent from `PATH`.

**An install we do not own is refused up front,** with the command to use
instead — not three network round-trips later with `permission denied`. That
means a package-manager prefix (`/usr/bin`, `/usr/sbin`, `/bin`, `/sbin`, the
Homebrew trees) or an install directory belonging to another user. Directory
ownership is what matters: the swap creates a temp file there and renames it,
so a directory owned by someone else fails regardless of the binary's mode.
Refusals are `*distkit.ErrManaged` (use `errors.As`), so a caller can tell
"you must not" from "it went wrong". `/usr/local/bin` is deliberately *not*
treated as package-manager owned — it is where this project's own `install.sh`
puts binaries — so a root-owned install there is refused by the ownership
check, which knows to suggest `sudo <tool> update` rather than "use your
package manager".

Off unix the whole design (directory ownership, rename over a running
executable, ETXTBSY) does not hold, so `checkOwnership` refuses outright
rather than performing an unverified overwrite. The module still compiles for
`GOOS=windows`.

## Config

| field | required | default | notes |
|---|---|---|---|
| `Repo` | ✓ | | GitHub `owner/name` |
| `Binary` | ✓ | | installed file name; also the name looked for inside an archive |
| `Version` | ✓ | | compiled-in version, with or without a leading `v` |
| `AssetStem` | | `Binary` | the `{stem}` in the asset name |
| `RestartHint` | | | printed after a successful swap, e.g. `systemctl --user reload zulip-acp` |
| `AssetTemplate` | | `{stem}-{os}-{arch}` | placeholders below; an archive suffix triggers unpacking |
| `ChecksumsAsset` | | `checksums.txt` | name of the manifest in the release |
| `SkipChecksums` | | `false` | install unverified (do not) |
| `ArmSuffix` | | `armv6` | the asset token for 32-bit ARM, where GOARCH is just `arm` |
| `DisableBrew` | | `false` | turn off Homebrew handling |
| `Token` | | discovered | `GITHUB_TOKEN` → `GH_TOKEN` → `gh auth token` |
| `APIBase` | | `https://api.github.com` | GitHub Enterprise, or a test double |
| `HTTPClient` | | see below | |
| `StallTimeout` | | 2 min | abandon a download that makes *no* progress for this long |
| `Stdout` / `Stderr` | | the process's | |
| `ExecPath` | | `os.Executable` | test seam |
| `Args` | | `os.Args[2:]` | flags after the `update` word |
| `TargetVersion` | | latest | pin a release |
| `CheckOnly` | | `false` | resolve and report only |

`AssetTemplate` placeholders: `{stem}`, `{binary}`, `{os}`, `{arch}`,
`{version}`, `{version_no_v}`. A rendered name ending in `.tar.gz`, `.tgz` or
`.zip` is unpacked — the file named `Binary` is pulled out at any depth, so
both a flat archive and harb's `harb-0.4.0-linux-arm64/harb` layout work.

The default HTTP client has **no total request deadline**: a release binary is
tens of megabytes and a Pi on a slow link legitimately takes minutes. Only the
parts that can actually hang are bounded — a 30 s `ResponseHeaderTimeout` for
a server that accepts the connection and never answers, and `StallTimeout` for
a transfer that dies mid-body. Slow is fine; no progress at all is not.

A **dev build is refused**. `Version: "dev"` (or `unknown`, `(devel)`,
`snapshot`, …) can never equal a tag, so every run would report an update
available and then rename a release binary over the developer's own build.
`distkit.IsDevBuild` is exported if you want to hide the subcommand instead.

Version comparison is exact tag equality, not "is newer". With
`TargetVersion` pinned the target may be *older* than what is installed, and a
deliberate downgrade is a thing an operator is allowed to ask for.

## The pieces, without the CLI

`Main` is a thin shell over exported functions, for callers (fir) that want
their own UI:

```go
st, err := distkit.Check(ctx, cfg)          // resolve + compare; touches no disk
dir, err := distkit.StagingDir(exe, "fir")  // temp dir beside the binary
defer os.RemoveAll(dir)
staged, err := distkit.Download(ctx, cfg, st.Release, dir) // fetch + verify + unpack
err = distkit.Apply(staged, exe)            // the atomic rename

err = distkit.CheckWritable(exe, "fir")     // the refusal, on its own
inst, err := distkit.DetectBrewInstall(ctx) // nil, nil when not brew-managed
err = distkit.UpgradeViaBrew(ctx, inst, "fir", os.Stdout, os.Stderr)

rel, err := distkit.FetchRelease(ctx, cfg, "v1.2.3") // rel.Assets is []distkit.Asset
tok := distkit.DiscoverToken()
sum, err := distkit.LookupChecksum(manifest, "fir-linux-amd64")
```

`distkit.Update(ctx, cfg)` is the whole flow, and is what `Main` calls.

## install.sh: generated and drift-checked

Shell cannot be imported, so the honest form of "shared" is
**generated-and-verified**. One canonical template lives in
`installsh/template.sh`; each consumer repo holds a small spec and a generated
`install.sh` at its root (it must be at the root for
`curl …/main/install.sh | sh` to resolve), plus a dev-only check that fails
when the checked-in copy has drifted.

`install.sh.json` in the consumer repo:

```json
{
  "repo": "kfet/harb",
  "binary": "harb",
  "asset_template": "{stem}-{version_no_v}-{os}-{arch}.tar.gz",
  "brew_formula": "kfet/tap/harb",
  "version_flag": "-version",
  "next_steps": ["harb init    # bootstrap config + password",
                 "harb serve   # start the server"]
}
```

Two Make targets:

```make
install.sh: install.sh.json
	go run github.com/kfet/distkit/cmd/distkit-installsh -o $@

check-installsh:
	go run github.com/kfet/distkit/cmd/distkit-installsh -check
```

`check-installsh` belongs in CI and in `make check`. It is dev-only and never
runs on a user's machine. Or do it from a test:

```go
func TestInstallShIsNotDrifted(t *testing.T) {
    spec, err := installsh.LoadSpec("install.sh.json")
    if err != nil { t.Fatal(err) }
    if err := installsh.CheckDrift("install.sh", spec); err != nil { t.Fatal(err) }
}
```

`installsh.FromConfig(cfg)` derives the spec from the same `distkit.Config`
the binary self-updates with, so asset naming has exactly one definition per
project.

### What the generated script does

The template is the union of the six hand-written scripts it replaces:

- `curl` **or** `wget`; only one of them needs to exist.
- `uname` mapping for linux/darwin (optionally freebsd), amd64/arm64
  (optionally 386), and all three 32-bit ARM spellings — `armv6l`, `armv7l`,
  `armv8l` — which collapse to the single GOARM=6 asset that every one of them
  can execute.
- Version resolution through the GitHub API, honouring `GITHUB_TOKEN`, so the
  same script works against a private repo. Without a token it uses the plain
  `releases/download` URL (one redirect, no JSON parsing); with one it picks
  the asset's API URL out of the release JSON, because the download host 404s
  a private repo whatever you send it.
- sha256 verification against `checksums.txt` (`sha256sum` or `shasum -a 256`).
- Optional tarball unpacking, finding the binary at any depth.
- `BIN_DIR`, with `PREFIX` accepted as the legacy alias (`$PREFIX/bin`) —
  the two schemes the old scripts disagreed on — defaulting to
  `/usr/local/bin` when writable and `$HOME/.local/bin` otherwise.
- Install via a sibling temp file and `mv -f`, so a running copy is replaced
  atomically; `sudo` escalation when the target directory is not writable.
- A `$PATH` warning, optional runtime-dependency warnings (acp-tmux needs
  `tmux` at runtime), an optional version smoke test, and optional next-step
  hints.

Spec fields: `repo`, `binary`, `asset_stem`, `asset_template`, `arm_suffix`,
`no_checksums`, `checksums_asset`, `darwin`, `freebsd`, `arch_386`,
`brew_formula`, `runtime_deps` (`[{name, message}]`), `version_flag`,
`next_steps`, `example_tag`. Unknown fields are rejected — a silently ignored
typo means a silently wrong installer.

Values that are interpolated into shell source (`repo`, `binary`,
`arm_suffix`, …) are rejected if they contain shell metacharacters. Values
that are prose (`next_steps`, `runtime_deps[].message`) are single-quoted
instead, so `"tmux is not on $PATH"` is a legal message.

Env overrides the generated script honours: `VERSION`, `BIN_DIR`, `PREFIX`,
`REPO`, `OS`, `ARCH`, `GITHUB_TOKEN`, `GITHUB_API`, `GITHUB_HOST`.

`til`'s installer is deliberately **not** in scope: it clones a repo and
installs through `pipx`, which is a different program that happens to share a
file name.

## Development

```
make check      # gofmt, vet, tests
make cover
```

The install.sh tests generate the script and actually run it against a fake
GitHub over `httptest`, for the raw-binary and tarball shapes, the anonymous
and tokenised download paths, `PREFIX`, and a checksum mismatch. `sh -n` alone
proves the script parses, not that it installs.
