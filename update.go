package distkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Check resolves the release that `update` would install and compares it with
// the running version. It touches nothing on disk, so it is safe to call from
// a status command or a background nag. Without a token it also spends no
// GitHub API quota — see FetchRelease.
//
// The comparison is exact-match on the tag, not "is newer": with
// Config.TargetVersion pinned the target may legitimately be older than what
// is installed, and calling a deliberate downgrade "no update available"
// would refuse to do a thing the operator explicitly asked for.
func Check(ctx context.Context, cfg Config) (*Status, error) {
	if err := cfg.normalise(); err != nil {
		return nil, err
	}
	rel, err := fetchRelease(ctx, &cfg, cfg.TargetVersion)
	if err != nil {
		return nil, err
	}
	cur := EnsureV(cfg.Version)
	target := EnsureV(rel.TagName)
	return &Status{
		Current:   cur,
		Target:    target,
		Available: target != cur,
		Release:   rel,
	}, nil
}

// Download fetches the release asset for the current platform into dir,
// verifies its sha256 against the release's checksum manifest, unpacks it if
// it is an archive, and returns the path of the executable ready to be
// installed.
//
// dir must be on the same filesystem as the binary that will be replaced for
// Apply to be atomic; StagingDir creates such a directory. The caller owns
// dir and is responsible for removing it.
//
// Verification is not optional in any sane configuration: the bytes are only
// trusted once the manifest agrees, and the manifest is fetched over the same
// authenticated API call chain as the asset itself.
func Download(ctx context.Context, cfg Config, rel *Release, dir string) (string, error) {
	if err := cfg.normalise(); err != nil {
		return "", err
	}
	if rel == nil {
		return "", fmt.Errorf("distkit: Download needs a resolved release")
	}
	asset := cfg.AssetName(rel.TagName)
	binURL, err := rel.AssetURL(asset)
	if err != nil {
		return "", err
	}

	// The raw-binary case stages straight to its final name and mode, so a
	// successful download needs no further processing before the rename.
	staged := filepath.Join(dir, asset)
	sum, err := fetch(ctx, &cfg, binURL, staged, 0o755)
	if err != nil {
		return "", fmt.Errorf("download asset: %w", err)
	}

	if !cfg.SkipChecksums {
		sumsURL, err := rel.AssetURL(cfg.ChecksumsAsset)
		if err != nil {
			return "", err
		}
		sumsPath := filepath.Join(dir, cfg.ChecksumsAsset)
		if _, err := fetch(ctx, &cfg, sumsURL, sumsPath, 0o644); err != nil {
			return "", fmt.Errorf("download checksums: %w", err)
		}
		manifest, err := os.ReadFile(sumsPath)
		if err != nil {
			return "", fmt.Errorf("read checksums: %w", err)
		}
		expected, err := LookupChecksum(manifest, asset)
		if err != nil {
			return "", err
		}
		if sum != expected {
			return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset, expected, sum)
		}
		fmt.Fprintln(cfg.Stdout, "✓ checksum verified")
	}

	if !isArchive(asset) {
		return staged, nil
	}
	unpacked := filepath.Join(dir, cfg.Binary)
	if err := extractBinary(staged, unpacked, cfg.Binary); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}
	// The archive itself is dead weight from here on, and on a small /home
	// partition a 20 MB tarball sitting next to the unpacked binary is not
	// free. Failure to remove it is harmless — dir is cleaned up anyway.
	_ = os.Remove(staged)
	return unpacked, nil
}

// StagingDir creates a temporary directory in the same directory as target,
// which is what makes the subsequent Apply an atomic same-filesystem rename.
// The caller must os.RemoveAll it.
func StagingDir(target, binary string) (string, error) {
	dir, err := os.MkdirTemp(filepath.Dir(target), "."+binary+"-update-*")
	if err != nil {
		return "", fmt.Errorf("tempdir: %w", err)
	}
	return dir, nil
}

// Apply replaces target with the file at staged.
//
// This is the ETXTBSY-safe swap. A running executable cannot be opened for
// writing or truncated — the kernel returns ETXTBSY — but its *directory
// entry* can be replaced. os.Rename does exactly that, atomically, and the
// process that is still running keeps the old inode mapped until it re-execs.
// That is why staged must be a sibling of target: a rename across filesystems
// degrades to copy-then-unlink, which is neither atomic nor permitted here.
func Apply(staged, target string) error {
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

// Update runs the whole flow: locate the running binary, hand a Homebrew
// install to brew, refuse an install we must not touch, then resolve,
// download, verify and swap.
//
// It returns Updated=false with a nil error for the two ordinary
// non-outcomes: already up to date, and CheckOnly.
func Update(ctx context.Context, cfg Config) (Result, error) {
	if err := cfg.normalise(); err != nil {
		return Result{}, err
	}
	var res Result
	res.Current = EnsureV(cfg.Version)

	// A dev build has no tag to compare against, so every run would report
	// an update and then rename a release binary over the developer's own.
	if IsDevBuild(cfg.Version) {
		return res, fmt.Errorf("%s %s is not a release build; nothing to update",
			cfg.Binary, cfg.Version)
	}

	resolved, err := resolveExe(&cfg)
	if err != nil {
		return res, err
	}
	res.ExecPath = resolved

	// Homebrew first. A keg install is not a failure mode — it is a
	// different, better update mechanism — and self-updating it would be
	// reverted by the next `brew upgrade`, leaving a host that reports one
	// version and runs another.
	if !cfg.DisableBrew {
		inst, err := detectBrewInstall(ctx, cfg.brewEnv())
		if err != nil {
			return res, err
		}
		if inst != nil {
			res.ViaBrew = true
			if cfg.CheckOnly {
				fmt.Fprintf(cfg.Stdout, "%s is a Homebrew install (%s); run `brew upgrade %s`\n",
					cfg.Binary, inst.ExePath, inst.Formula)
				return res, nil
			}
			fmt.Fprintf(cfg.Stdout, "homebrew install detected: upgrading %s\n", inst.Formula)
			if err := UpgradeViaBrew(ctx, inst, cfg.Binary, cfg.Stdout, cfg.Stderr); err != nil {
				return res, err
			}
			res.Updated = true
			return res, nil
		}
	}

	// Refuse before touching the network — except under CheckOnly, where the
	// operator asked a question ("is there a newer release?") that is worth
	// answering even on a host we may not write to.
	writeErr := CheckWritable(resolved, cfg.Binary)
	if writeErr != nil && !cfg.CheckOnly {
		return res, writeErr
	}

	st, err := Check(ctx, cfg)
	if err != nil {
		return res, err
	}
	res.Current, res.Target = st.Current, st.Target

	// "target", not "latest": with -version pinned this may be older than
	// what is installed, and calling a downgrade "latest" would be a lie.
	fmt.Fprintf(cfg.Stdout, "current: %s\ntarget:  %s\n", st.Current, st.Target)
	if !st.Available {
		fmt.Fprintln(cfg.Stdout, "already up to date.")
		return res, nil
	}
	if cfg.CheckOnly {
		fmt.Fprintf(cfg.Stdout, "update available: %s → %s. run `%s update` to install.\n",
			st.Current, st.Target, cfg.Binary)
		if writeErr != nil {
			fmt.Fprintf(cfg.Stdout, "note: %v\n", writeErr)
		}
		res.Available = true
		return res, nil
	}

	fmt.Fprintf(cfg.Stdout, "downloading %s %s\n", cfg.AssetName(st.Target), st.Target)
	dir, err := StagingDir(resolved, cfg.Binary)
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)

	staged, err := Download(ctx, cfg, st.Release, dir)
	if err != nil {
		return res, err
	}
	if err := Apply(staged, resolved); err != nil {
		return res, err
	}
	res.Updated = true
	fmt.Fprintf(cfg.Stdout, "✓ %s updated: %s → %s at %s\n", cfg.Binary, st.Current, st.Target, resolved)
	return res, nil
}

// brewEnv builds the detection environment, honouring Config.ExecPath so a
// test can drive the brew branch over a synthetic Cellar path.
func (c *Config) brewEnv() brewEnv {
	env := defaultBrewEnv()
	if c.ExecPath != nil {
		env.executablePath = c.ExecPath
	}
	return env
}
