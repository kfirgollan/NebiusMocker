package dockerhost

import (
	"context"
	"fmt"
	"net"
)

// FreePort returns a TCP port nothing listens on at ip right now. Backends
// use it to pin a container's published port, so it survives restarts.
func FreePort(ctx context.Context, ip string) (string, error) {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		return "", fmt.Errorf("docker: pick a free port on %s: %w", ip, err)
	}
	defer func() { _ = l.Close() }() // best-effort: the probe listener only reserves the number
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}
