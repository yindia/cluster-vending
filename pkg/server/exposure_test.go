package server_test

import (
	"net"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/server"
)

func TestExposedWithoutAuth(t *testing.T) {
	tests := []struct {
		name string
		mode auth.Mode
		addr net.Addr
		want bool
	}{
		{"none on every interface", auth.ModeNone, &net.TCPAddr{IP: net.IPv6unspecified, Port: 8080}, true},
		{"empty mode on a pod IP", "", &net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 8080}, true},
		{"none on 127.0.0.1", auth.ModeNone, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}, false},
		{"none on ::1", auth.ModeNone, &net.TCPAddr{IP: net.IPv6loopback, Port: 8080}, false},
		{"none on a unix socket", auth.ModeNone, &net.UnixAddr{Name: "/tmp/api.sock", Net: "unix"}, true},
		{"required on every interface", auth.ModeRequired, &net.TCPAddr{IP: net.IPv6unspecified, Port: 8080}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := server.ExposedWithoutAuth(tt.mode, tt.addr); got != tt.want {
				t.Errorf("ExposedWithoutAuth(%q, %v) = %t, want %t", tt.mode, tt.addr, got, tt.want)
			}
		})
	}
}
