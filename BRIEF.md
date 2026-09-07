# distkit — build brief

## What this is

`github.com/kfet/distkit` — one standalone Go module that owns **binary
distribution and self-update** for the whole family of kfet tools.

Today there are **four hand-written self-update implementations**, each
missing a different piece, and **six divergent `install.sh` scripts** that
are not template clones. This module replaces all of them.

### Hard constraints

- **No dependency on ACP, on any relay, or on fir.** This module must be
  importable by `fir`, `harb`, `mintick` and the three relays alike. That is
  precisely why it is not going into `acp-kit`.
- **Stdlib only** unless there is a compelling reason. Go 1.25 (the family
  baseline; toolchain here is 1.26.4).
- Public repo. `gh repo create kfet/distkit --public` when the code is ready.

## Public API

One call per consumer. Everything else is a config field.

```go
distkit.Main(distkit.Config{
    Repo:        "kfet/zulip-acp",   // GitHub slug
    Binary:      "zulip-acp",        // installed binary name
    AssetStem:   "zulip-acp",        // release asset is <stem>-<os>-<arch>
    Version:     version,            // compiled-in current version
    RestartHint: "systemctl --user reload zulip-acp",
})
```

`Main` is the CLI entry point for an `update` subcommand. Expose the
underlying pieces (`Check`, `Download`, `Apply`, brew detection) as
separately callable functions too — fir will want them without the CLI.

## Merge these four sources — best of each

| source | LOC | take this |
|---|---|---|
| `~/src/zulip-acp/internal/selfupdate` | 1210 (incl. tests) | **The core.** GitHub **API** fetch with token — the only implementation that works against a private repo. sha256 against `checksums.txt`. ETXTBSY-safe atomic swap (rename over a running binary). Also has `main.go` = the CLI shape. |
| `~/src/fir/pkg/update/brew.go` | 257 + tests | **Brew detect *and* upgrade.** Resolves the exe through symlinks, trusts the three standard prefixes (`/opt/homebrew`, `/usr/local`, `/home/linuxbrew/.linuxbrew`), resolves the fully-qualified formula (`kfet/ai/fir`) from the keg receipt, validates the tap name, then runs `brew update && brew upgrade <formula>` with no timeout. Refuses only when clearly brew-managed but `brew` is not on PATH. |
| `~/src/poe-acp/internal/selfupdate` | 707 | Cross-check only. Its brew path merely *refuses* — do not carry that over. |
| `~/src/harb/internal/selfupdate` | 698 | Cross-check only. Same refuse-only brew behaviour. |

**Do not lose:** the GitHub-API-with-token path. It is the piece poe-acp's
and harb's copies lack, and it is required while a repo is private. This was
called out explicitly in `zulip-acp/BACKLOG.md:14`.

The four differ in small ways beyond the headline features — read all four
before writing, and where they disagree on a detail, prefer the behaviour
that is safest on a running production host.

## Second half of the scope: `install.sh`

Shell cannot be imported, so the honest version of "shared" is
**generated-and-verified**:

- One canonical template lives in this repo.
- Each consumer repo's root `install.sh` is *generated* from it (the file
  must sit at the repo root for `curl …/main/install.sh | sh` to resolve).
- A **dev-only drift check** — a Make target or a test — fails if a
  consumer's copy no longer matches what the template would generate.

Current copies, all divergent (different variable names — `PREFIX` vs
`BIN_DIR` — different override schemes, only some with a curl-or-wget
fallback):

```
harb       104   mintick   55   airan     59
acp-tmux   148   slack-acp 133   til      347
zulip-acp  MISSING   poe-acp  MISSING   fir  MISSING
```

Read all six before designing the template. The template must cover the
union of what they do, parameterised.

## Scope of THIS agent

Phase 1 only — build and publish the module. Do **not** touch consumer
repos; that is a follow-up worktree.

1. Read all four Go sources and all six `install.sh` copies.
2. Design and write `distkit`: the library, the `Main` CLI, the brew path,
   the `install.sh` template + generator + drift check.
3. Real tests. The four sources ship substantial test suites — mine them,
   do not start from zero.
4. `README.md` — the API, the config fields, how a consumer wires it up in
   one call, and how the `install.sh` generation + drift check works.
5. `gh repo create kfet/distkit --public --source=. --push`, then tag `v0.1.0`.
6. Delete this `BRIEF.md` in the final commit.

## Explicitly out of scope for now

- Wiring consumers. Order when it happens: **zulip-acp → poe-acp → harb →
  slack-acp**; `fir` last or never (it uses `go-selfupdate` and is the one
  implementation that is not obviously worse).
- Deleting `internal/selfupdate` from zulip-acp / poe-acp / harb.
- `zulip-acp/BACKLOG.md:14`, which still names `acp-kit` as the home — that
  is now wrong and gets fixed in the zulip-acp pass.

Design so those follow-ups are a small diff each: one import, one `Main`
call, one deleted package.
