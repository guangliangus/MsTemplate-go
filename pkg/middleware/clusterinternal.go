package middleware

import (
	"net"
	"net/http"
	"strings"

	"example.com/msvcservice/pkg/httpx"
)

// forwardedHeaders are what every reverse proxy in the platform's stack adds. A direct pod-to-pod
// call from a Go client sets none of them, which is what separates "came in from the internet"
// from "another service called us over the cluster network" — more reliably than the peer address.
var forwardedHeaders = []string{
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-Ip", "Forwarded",
}

// ClusterInternalOnly guards a route group that carries no credential of its own (an
// /api/v1/internal group whose X-Service-Name is a tracing header, not a credential) and is meant
// to be reachable only from inside the cluster, with the ingress blocking it from outside. It is
// the belt to that braces:
//
//   - a request carrying any forwarded header is refused: it passed through a proxy, so it came
//     from outside. This is the load-bearing check, and it still holds when an ingress rule is
//     mis-scoped, because the proxy cannot route here without announcing itself in those headers;
//   - the TCP peer must be a private, loopback or link-local address: a cheap second signal, not
//     the control (a proxied request has the proxy's pod as its peer, which is private too);
//   - it is NOT authorization: anything that can open a connection to the pod without a proxy in
//     between is trusted. A route that must tell internal callers apart needs a credential.
//
// The peer is Request.RemoteAddr, deliberately not ClientIP(): the resolved client address comes
// from X-Forwarded-For, which is exactly what an attacker would forge here.
func ClusterInternalOnly() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, h := range forwardedHeaders {
				if r.Header.Get(h) != "" {
					httpx.ErrorStatus(w, r, http.StatusForbidden,
						"Requests forwarded by a proxy are not accepted on this route.")
					return
				}
			}
			if !isInternalPeer(r.RemoteAddr) {
				httpx.ErrorStatus(w, r, http.StatusForbidden, "This route is only reachable from inside the cluster.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isInternalPeer reports whether the TCP peer is a private, loopback, link-local or unspecified
// address. A peer that cannot be parsed is external: failing closed is the only safe direction for
// a route without a credential.
func isInternalPeer(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // an IPv6 zone (fe80::1%eth0) is not part of the address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}
