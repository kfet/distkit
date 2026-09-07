package distkit

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
)

// Main implements the `<tool> update` subcommand and returns the process exit
// code. It is the whole integration surface for a consumer:
//
//	case "update":
//	    os.Exit(distkit.Main(distkit.Config{
//	        Repo:        "kfet/zulip-acp",
//	        Binary:      "zulip-acp",
//	        Version:     version,
//	        RestartHint: "systemctl --user reload zulip-acp",
//	    }))
//
// Flags parsed from Config.Args (default os.Args[2:], i.e. everything after
// the subcommand word):
//
//	-check              report whether an update is available; install nothing
//	-version <tag>      install a specific release instead of the latest
//	-repo <owner/name>  override the source repository
//	-restart-cmd <sh>   run this after a successful update
//
// Exit codes: 0 success (including "already up to date"), 1 the update
// failed or was refused, 2 the flags did not parse, and
// ExitUpdateAvailable (3) for a -check run that found a newer release.
// ExitUpdateAvailable is the exit code of a `-check` run that found a release
// different from the one running. It is distinct from 0 ("already up to
// date") so a scheduled check can act on the result without parsing stdout.
const ExitUpdateAvailable = 3

func Main(cfg Config) int {
	args := cfg.Args
	if args == nil {
		args = os.Args[min(2, len(os.Args)):]
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	cfg.Stdout, cfg.Stderr = stdout, stderr

	name := cfg.Binary
	if name == "" {
		name = "update"
	}
	fs := flag.NewFlagSet(name+" update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", cfg.CheckOnly, "report whether an update is available; do not install")
	ver := fs.String("version", cfg.TargetVersion, "install a specific version (e.g. v0.19.0); default: latest")
	repo := fs.String("repo", cfg.Repo, "github owner/repo to update from")
	restartHelp := "shell command to run after a successful update"
	if cfg.RestartHint != "" {
		restartHelp += fmt.Sprintf("; prefer the graceful reload, e.g. %q", cfg.RestartHint)
	}
	restartCmd := fs.String("restart-cmd", "", restartHelp)
	if err := fs.Parse(args); err != nil {
		// `-h` is a successful request for help, not a usage error.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg.Repo, cfg.TargetVersion, cfg.CheckOnly = *repo, *ver, *check

	res, err := Update(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(stderr, "update: %v\n", err)
		// A refusal is a correct, expected outcome on a managed host, not a
		// crash; it still exits non-zero so a script notices, but it must
		// not read like a bug report.
		var managed *ErrManaged
		if errors.As(err, &managed) && managed.Hint != "" {
			fmt.Fprintf(stderr, "nothing was changed.\n")
		}
		return 1
	}
	// A -check run reports through the exit code as well as stdout, so a
	// timer or a cron nag can act on it without parsing text.
	if res.Available {
		return ExitUpdateAvailable
	}
	if !res.Updated || res.ViaBrew {
		// brew already left a working binary on disk and printed its own
		// account of what it did; there is no staged swap to recycle for.
		return 0
	}

	// A swapped binary is inert until the process re-execs: the running one
	// still has the old inode mapped.
	if *restartCmd == "" {
		if cfg.RestartHint != "" {
			fmt.Fprintf(stdout, "recycle it to run the new binary:\n  %s\n", cfg.RestartHint)
		}
		return 0
	}
	fmt.Fprintf(stdout, "recycling: %s\n", *restartCmd)
	c := exec.Command("sh", "-c", *restartCmd)
	c.Stdout, c.Stderr = stdout, stderr
	if err := c.Run(); err != nil {
		fmt.Fprintln(stderr, "recycle:", err)
		return 1
	}
	return 0
}
