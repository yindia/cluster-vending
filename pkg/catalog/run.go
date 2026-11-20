package catalog

import (
	"context"
	"fmt"
	"io"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RunCheck is `controller --check -f <path|->`: it reads the manifests from
// r, prints every problem to w, one line each, and returns an error that
// says how many when there are any (the command then exits 1). It makes no
// cluster call.
func RunCheck(r io.Reader, w io.Writer, opts Options) error {
	set, problems := Decode(r)
	problems = append(problems, Check(set, opts)...)
	if len(problems) == 0 {
		_, err := fmt.Fprintf(w, "catalog ok: %d definitions, %d releases, %d channels\n",
			len(set.Definitions), len(set.Releases), len(set.Channels))
		return err
	}
	for _, p := range problems {
		if _, err := fmt.Fprintln(w, p); err != nil {
			return err
		}
	}
	if len(problems) == 1 {
		return fmt.Errorf("the catalog has 1 problem")
	}
	return fmt.Errorf("the catalog has %d problems", len(problems))
}

// RunExport is `controller --export`: it reads the management cluster and
// writes the catalog as manifests, to dir (config/catalog's layout) or, when
// dir is empty, as one stream to out. On stderr it lists how the live
// definitions differ from this build's embedded catalog. It only reads.
func RunExport(ctx context.Context, c client.Reader, dir string, out, stderr io.Writer) error {
	set, err := Export(ctx, c)
	if err != nil {
		return err
	}
	differences, err := Differences(set)
	if err != nil {
		return fmt.Errorf("compare with this build's catalog: %w", err)
	}
	for _, d := range differences {
		if _, err := fmt.Fprintln(stderr, "differs from the build:", d); err != nil {
			return err
		}
	}
	if dir == "" {
		return set.WriteStream(out)
	}
	if err := set.WriteDir(dir); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stderr, "exported %d definitions, %d releases and %d channels to %s\n",
		len(set.Definitions), len(set.Releases), len(set.Channels), dir)
	return err
}
