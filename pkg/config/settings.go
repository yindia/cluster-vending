package config

import "fmt"

// LookupFunc returns a value by key and whether it is set, like
// os.LookupEnv. For flags, "set" means given on the command line.
type LookupFunc func(key string) (string, bool)

// Setting is one configuration value: its command-line flag, the
// environment variable that backs it, and its default.
type Setting struct {
	Flag    string // without the leading "--"
	Env     string
	Default string
	Usage   string
	// Key is the setting's place in the config file's nodeGroups section,
	// when it has one; messages name it first.
	Key string
}

// String names the setting the way errors report it.
func (s Setting) String() string {
	if s.Key != "" {
		return fmt.Sprintf("%s (--%s, %s)", s.Key, s.Flag, s.Env)
	}
	return fmt.Sprintf("--%s (%s)", s.Flag, s.Env)
}

// resolve returns the value of every setting, keyed by flag name: the flag
// if given, else the environment variable, else the config file (keyed by
// flag name, see LoadFile), else the default. A flag, variable or file
// entry that is set but empty is an error, not a request for the default.
// A nil lookup is treated as having nothing set.
func resolve(settings []Setting, flags, env, file LookupFunc) (map[string]string, []error) {
	values := make(map[string]string, len(settings))
	var errs []error
	for _, s := range settings {
		v, ok := get(flags, s.Flag)
		if !ok {
			v, ok = get(env, s.Env)
		}
		if !ok {
			v, ok = get(file, s.Flag)
		}
		if !ok {
			v = s.Default
		} else if v == "" {
			errs = append(errs, fmt.Errorf("%s is set but empty", s))
		}
		values[s.Flag] = v
	}
	return values, errs
}

func get(lookup LookupFunc, key string) (string, bool) {
	if lookup == nil {
		return "", false
	}
	return lookup(key)
}
