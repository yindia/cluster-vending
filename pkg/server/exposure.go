package server

import (
	"net"

	"github.com/yindia/cluster-vending/pkg/auth"
)

// ExposedWithoutAuth reports whether the API serves every caller
// anonymously (auth mode none) on an address other hosts can reach: any
// address but a loopback one (an unspecified address such as ":8080"
// listens on every interface). The server still starts (local kind needs
// it) but logs a warning.
func ExposedWithoutAuth(mode auth.Mode, addr net.Addr) bool {
	if mode != auth.ModeNone && mode != "" {
		return false
	}
	tcp, ok := addr.(*net.TCPAddr)
	return !ok || !tcp.IP.IsLoopback()
}
