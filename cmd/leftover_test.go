package cmd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

func TestWarnLeftoverNodeGroupVariables(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, nil))
	env := func(k string) (string, bool) { return "8", k == config.EnvSystemNodeGroupMaxSize }
	warnLeftoverNodeGroupVariables(logger, env)
	if got := out.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, config.EnvSystemNodeGroupMaxSize) {
		t.Errorf("log = %q", got)
	}
	out.Reset()
	warnLeftoverNodeGroupVariables(logger, func(string) (string, bool) { return "", false })
	if out.Len() != 0 {
		t.Errorf("nothing set, logged %q", out.String())
	}
}

// The keys on their way out (nodeGroups.system.maxSize, .instanceType) are
// warned about at start of serve and controller; nothing is logged without.
func TestWarnNodeGroups(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, nil))
	warnNodeGroups(logger, config.NodeGroups{Warnings: []string{"nodeGroups.system.maxSize is ignored"}})
	if got := out.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "nodeGroups.system.maxSize") {
		t.Errorf("log = %q", got)
	}
	out.Reset()
	warnNodeGroups(logger, config.DefaultNodeGroups())
	if out.Len() != 0 {
		t.Errorf("defaults logged %q", out.String())
	}
}
