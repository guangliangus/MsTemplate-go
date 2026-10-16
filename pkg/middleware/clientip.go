package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ForwardedConfig is bound from "http.forwarded_headers".
type ForwardedConfig struct {
	// Enabled honours X-Forwarded-For. Off by default; the chart turns it on, because behind the
	// ingress RemoteAddr is a node address, which would ruin IP rate-limit partitions and logs.
	Enabled bool `koanf:"enabled"`
	// TrustedProxies are CIDRs of proxies whose X-Forwarded-For entries are skipped when walking the
	// header from the right. Empty trusts every proxy (the rightmost entry is the client), which
	// means any in-cluster caller could spoof its address: set the pod CIDR to tighten.
	TrustedProxies []string `koanf:"trusted_proxies"`
}

// Validate parses the CIDRs.
func (c *ForwardedConfig) Validate() error {
	_, err := parsePrefixes(c.TrustedProxies)
	return err
}

func parsePrefixes(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %q is not a CIDR: %w", c, err)
		}
		out = append(out, p)
	}
	return out, nil
}

type clientIPKey struct{}

// ClientIPResolver resolves the real client address and stores it on the context. It must be the
// first middleware: everything after it (rate limiting, logging) reads ClientIP.
func ClientIPResolver(cfg ForwardedConfig) func(http.Handler) http.Handler {
	trusted, _ := parsePrefixes(cfg.TrustedProxies)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := remoteIP(r)
			if cfg.Enabled {
				if forwarded := forwardedClient(r.Header.Values("X-Forwarded-For"), trusted); forwarded != "" {
					ip = forwarded
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip)))
		})
	}
}

// ClientIP returns the resolved client address, or "" when the request has none (a test server,
// a connection the host cannot attribute).
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return remoteIP(r)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return ""
	}
	return host
}

// forwardedClient walks the X-Forwarded-For chain from the right (the entry the last proxy
// appended, which the client could not forge) and skips trusted proxies.
func forwardedClient(values []string, trusted []netip.Prefix) string {
	var entries []string
	for _, v := range values {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(entries[i])
		if err != nil {
			return ""
		}
		if i > 0 && inAny(addr, trusted) {
			continue
		}
		return addr.String()
	}
	return ""
}

func inAny(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
