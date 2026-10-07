package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/showmino/forge/internal/exitcode"
)

// confirmDestructive asks the user to type name before a destructive action.
// --yes skips it. Without a terminal (or with --no-input) it fails with exit
// code 2 rather than hanging.
func (f *Factory) confirmDestructive(action, name string, yes bool) error {
	if yes {
		return nil
	}
	if !f.interactive() {
		return exitcode.Usagef("refusing to %s %s without confirmation", action, name).
			WithHint("pass --yes to confirm, or --dry-run to preview")
	}
	fmt.Fprintf(f.IO.ErrOut, "This will %s %s. Type its name to confirm: ", action, name)
	line, err := bufio.NewReader(f.IO.In).ReadString('\n')
	if err != nil && line == "" {
		return exitcode.Usagef("no confirmation received")
	}
	if strings.TrimSpace(line) != name && strings.TrimSpace(line) != shortName(name) {
		return exitcode.Usagef("confirmation did not match %q; nothing was changed", shortName(name))
	}
	return nil
}

// shortName strips the collection prefix: "services/payments" -> "payments".
func shortName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}
