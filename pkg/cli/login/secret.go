package login

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// ReadClientSecret reads at most one explicit private source. A nil prompt and
// empty source names select a public client. It never includes secret contents
// in an error, nor accepts secret text as a CLI argument.
func ReadClientSecret(envName, path string, prompt func() (string, error)) (string, error) {
	count := 0
	if envName != "" {
		count++
	}
	if path != "" {
		count++
	}
	if prompt != nil {
		count++
	}
	if count > 1 {
		return "", errors.New("choose one client secret source: environment, private file or terminal prompt")
	}
	if count == 0 {
		return "", nil
	}
	var secret string
	switch {
	case envName != "":
		var found bool
		secret, found = os.LookupEnv(envName)
		if !found {
			return "", fmt.Errorf("client secret environment variable %s is not set", envName)
		}
	case path != "":
		info, err := os.Lstat(path)
		if err != nil {
			return "", errors.New("cannot read client secret file")
		}
		if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("client secret file must be a private regular file (mode 0600 or 0400), not a symbolic link")
		}
		file, err := os.Open(path)
		if err != nil {
			return "", errors.New("cannot open client secret file")
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 65537))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(raw) > 65536 {
			return "", errors.New("cannot read client secret file, or it exceeds 64 KiB")
		}
		secret = strings.TrimRight(string(raw), "\r\n")
	case prompt != nil:
		var err error
		secret, err = prompt()
		if err != nil {
			return "", errors.New("could not read client secret from terminal")
		}
	}
	if secret == "" {
		return "", errors.New("selected client secret source is empty")
	}
	return secret, nil
}
