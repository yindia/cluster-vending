package cli

import "fmt"

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
