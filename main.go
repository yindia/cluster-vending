// Command cluster-vending is the cluster-vending CLI and API server.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/yindia/cluster-vending/cmd"
)

func main() {
	if err := cmd.Execute(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
