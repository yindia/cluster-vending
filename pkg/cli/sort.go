package cli

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// enumFlag maps a flag value to an enum number: the value name without
// prefix, in lower case with dashes (NODE_SORT_BY_CPU_REQUESTED_PERCENT is
// cpu-requested-percent). Empty is 0 (UNSPECIFIED: the server's default).
// An unknown value is refused with every valid one listed.
func enumFlag(flag, value, prefix string, enum protoreflect.EnumDescriptor) (protoreflect.EnumNumber, error) {
	if value == "" {
		return 0, nil
	}
	want := strings.ToLower(strings.TrimSpace(value))
	var valid []string
	values := enum.Values()
	for i := range values.Len() {
		v := values.Get(i)
		if v.Number() == 0 {
			continue
		}
		name := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(string(v.Name()), prefix)), "_", "-")
		if name == want {
			return v.Number(), nil
		}
		valid = append(valid, name)
	}
	return 0, fmt.Errorf("--%s %q: want one of %s", flag, value, strings.Join(valid, ", "))
}

// sortOrder is --order: asc or desc; empty is the list's default.
func sortOrder(value string) (cloudv1.SortOrder, error) {
	n, err := enumFlag("order", value, "SORT_ORDER_", cloudv1.SortOrder(0).Descriptor())
	return cloudv1.SortOrder(n), err
}

// sortOf parses --sort with the enum E of the list and --order.
func sortOf[E interface {
	~int32
	Descriptor() protoreflect.EnumDescriptor
}](o Options, prefix string) (E, cloudv1.SortOrder, error) {
	var zero E
	by, err := enumFlag("sort", o.SortBy, prefix, zero.Descriptor())
	if err != nil {
		return 0, 0, err
	}
	order, err := sortOrder(o.SortOrder)
	if err != nil {
		return 0, 0, err
	}
	return E(by), order, nil
}
