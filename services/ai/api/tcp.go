package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

// serveTCP accepts TLS connections for endpoints' TCP ports until ctx ends.
// Each connection is routed by SNI, gets one line and is closed:
//
//	endpoint was called: <endpoint-id> port <port>
//	error 404|503: <reason>
func (d *DataPlane) serveTCP(ctx context.Context, ln net.Listener) {
	var conns sync.WaitGroup
	defer conns.Wait()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() }) // unblocks Accept
	defer stop()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				d.log.ErrorContext(ctx, "ai tcp stub stopped", "error", err)
			}
			return
		}
		conns.Go(func() { d.handleTCP(ctx, c) })
	}
}

func (d *DataPlane) handleTCP(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }() // one-shot connection
	tc, ok := c.(*tls.Conn)
	if !ok {
		return
	}
	// A client that never finishes the handshake is cut off on shutdown.
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := tc.HandshakeContext(ctx); err != nil {
		return
	}
	d.mu.Lock()
	calls, domain := d.calls, d.tcpDomain
	d.mu.Unlock()
	_, _ = io.WriteString(c, d.tcpLine(ctx, calls, domain, tc.ConnectionState().ServerName)) // the client may be gone
}

func (d *DataPlane) tcpLine(ctx context.Context, calls CallAuthorizer, domain, serverName string) string {
	id, port, ok := parseTCPHost(serverName, domain)
	if !ok {
		return "error 404: no endpoint in TLS server name; connect to <endpoint-id>-p<port>." + domain + "\n"
	}
	call, err := calls.AuthorizeTCP(ctx, id, port)
	if err != nil {
		return fmt.Sprintf("error %d: %s\n", statusOf(err), errorMessage(err))
	}
	return fmt.Sprintf("endpoint was called: %s port %d\n", call.EndpointID, call.Port)
}

// parseTCPHost splits "<endpoint-id>-p<port>.<domain>" (case-insensitive
// domain) into the endpoint ID and port.
func parseTCPHost(host, domain string) (endpointID string, port int32, ok bool) {
	suffix := "." + domain
	if len(host) <= len(suffix) || !strings.EqualFold(host[len(host)-len(suffix):], suffix) {
		return "", 0, false
	}
	label := host[:len(host)-len(suffix)]
	i := strings.LastIndex(label, "-p")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(label[i+2:], 10, 32)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return label[:i], int32(n), true
}
