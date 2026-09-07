// Command distkit-installsh generates a repo's root install.sh from the
// canonical distkit template, or checks the checked-in copy for drift.
//
// A consumer repo keeps a spec file (install.sh.json by default) at its root
// and two Make targets:
//
//	install.sh: install.sh.json
//		go run github.com/kfet/distkit/cmd/distkit-installsh -o $@
//
//	check-installsh:
//		go run github.com/kfet/distkit/cmd/distkit-installsh -check
//
// The check target belongs in CI and in `make check`; it is dev-only and
// never runs on a user's machine.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kfet/distkit/installsh"
)

func main() {
	spec := flag.String("spec", "install.sh.json", "path to the install.sh spec")
	out := flag.String("o", "install.sh", "path to write (or, with -check, to verify)")
	check := flag.Bool("check", false, "verify the existing file matches the template instead of writing it")
	flag.Parse()

	s, err := installsh.LoadSpec(*spec)
	if err != nil {
		fail(err)
	}
	if *check {
		if err := installsh.CheckDrift(*out, s); err != nil {
			fail(err)
		}
		fmt.Printf("%s is up to date with the distkit template\n", *out)
		return
	}
	if err := installsh.Write(*out, s); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s\n", *out)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "distkit-installsh:", err)
	os.Exit(1)
}
