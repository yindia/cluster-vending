// Package version reports the build version of cluster-vending. The values
// are set at build time, e.g.:
//
//	go build -ldflags "-X github.com/yindia/cluster-vending/pkg/version.Version=v0.1.0 \
//	  -X github.com/yindia/cluster-vending/pkg/version.Commit=$(git rev-parse HEAD)"
package version

// Set through -ldflags -X; the defaults mark a development build.
var (
	Version = "dev"
	Commit  = "unknown"
)
