package cli

import (
	"fmt"
	"strings"
)

// ResolveID is the ID a command was given: --id (flag) or its argument;
// both are accepted when they agree.
func ResolveID(flag string, args []string) (string, error) {
	if len(args) == 0 {
		return flag, nil
	}
	if flag != "" && flag != args[0] {
		return "", fmt.Errorf("--id %s and the argument %s differ; give the ID once", flag, args[0])
	}
	return args[0], nil
}

// IDVerb is the usage of a verb that acts on one object by its ID, written
// as "get <id>" (required) or "update [id]" (optional): the command takes
// --id instead, so its usage drops the argument. A positional ID is still
// accepted (ResolveID). Any other usage is returned as it is.
func IDVerb(use string) (usage string, takesID, required bool) {
	name, rest, _ := strings.Cut(use, " ")
	switch strings.TrimSpace(rest) {
	case "<id>":
		return name, true, true
	case "[id]":
		return name, true, false
	}
	return use, false, false
}

// RequireID refuses an empty ID of a noun's command.
func RequireID(id, noun string) error {
	if id == "" {
		return fmt.Errorf("--id is required: the %s's ID", noun)
	}
	return nil
}
