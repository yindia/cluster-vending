package catalog

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
)

// PinValueProblems reports the component pins of a release whose values
// are unusable: not an object, or naming an unknown placeholder.
func PinValueProblems(content cloudv1alpha1.ReleaseContent) []string {
	var problems []string
	for _, k := range content.Kubernetes {
		for _, c := range k.Components {
			if _, err := component.PinValues(c.Values); err != nil {
				problems = append(problems, fmt.Sprintf("on %s, the values of %s: %v", k.Minor, c.Name, err))
			}
		}
	}
	return problems
}

// ValidateRelease makes the checks of a release's content the API
// server's CEL rules cannot: the minors are consecutive, an upgradeTo is
// the next minor and one of them, the channels are each on one minor
// from the newest (alpha) to the oldest (stable) or none is
// (release.ChannelProblems), and the renderer version is not newer than
// this build's (render). It returns the problems found; none means valid.
// The controller's Valid condition and the offline check use it.
func ValidateRelease(content cloudv1alpha1.ReleaseContent, render int64) []string {
	var problems []string
	if content.RenderVersion > render {
		problems = append(problems, fmt.Sprintf("renderVersion %d is newer than this controller's renderer (%d)", content.RenderVersion, render))
	}
	minors := map[string]bool{}
	var numbers []int
	for _, k := range content.Kubernetes {
		n, ok := minorNumber(k.Minor)
		if !ok {
			problems = append(problems, fmt.Sprintf("minor %q is not 1.<n>", k.Minor))
			continue
		}
		minors[k.Minor] = true
		numbers = append(numbers, n)
	}
	slices.Sort(numbers)
	for i := 1; i < len(numbers); i++ {
		if numbers[i] != numbers[i-1]+1 {
			problems = append(problems, fmt.Sprintf("the minors are not consecutive: 1.%d is followed by 1.%d", numbers[i-1], numbers[i]))
		}
	}
	for _, k := range content.Kubernetes {
		if k.UpgradeTo == "" {
			continue
		}
		from, okFrom := minorNumber(k.Minor)
		to, okTo := minorNumber(k.UpgradeTo)
		switch {
		case !okFrom || !okTo || to != from+1:
			problems = append(problems, fmt.Sprintf("%s upgrades to %s: an upgrade goes to the next minor", k.Minor, k.UpgradeTo))
		case !minors[k.UpgradeTo]:
			problems = append(problems, fmt.Sprintf("%s upgrades to %s, which the release does not have", k.Minor, k.UpgradeTo))
		}
	}
	problems = append(problems, release.ChannelProblems(reconcile.ReleaseOf("", content))...)
	return append(problems, PinValueProblems(content)...)
}

// minorNumber is n of "1.<n>".
func minorNumber(minor string) (int, bool) {
	rest, ok := strings.CutPrefix(minor, "1.")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}
