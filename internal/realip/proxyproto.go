package realip

import (
	"fmt"
	"net"

	"github.com/pires/go-proxyproto"
)

// WrapProxyProto wraps ln so that connections from a trusted proxy range
// (a TCP-level load balancer speaking the HAProxy PROXY protocol — not
// Traefik, which can't proxy raw SMTP/IMAP at all) have their PROXY header
// parsed and RemoteAddr() replaced with the real client address it carries.
//
// Connections from any other source are passed through completely
// unexamined (Policy SKIP): with no trusted_proxies configured — the
// default, matching the current deployment where nothing sits in front of
// these ports — ln is returned unwrapped, so this adds no behavior at all.
func WrapProxyProto(ln net.Listener, trustedProxies []string) (net.Listener, error) {
	if len(trustedProxies) == 0 {
		return ln, nil
	}

	policy, err := proxyproto.PolicyFromRanges(trustedProxies, proxyproto.USE, proxyproto.SKIP)
	if err != nil {
		return nil, fmt.Errorf("invalid trusted_proxies for PROXY protocol: %w", err)
	}

	return &proxyproto.Listener{Listener: ln, ConnPolicy: policy}, nil
}
