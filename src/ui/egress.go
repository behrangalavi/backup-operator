package ui

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"backup-operator/internal/secrets"
)

// blockedMetadataV6 is the AWS IPv6 IMDS endpoint. It lives in the ULA range
// (fc00::/7), so the generic link-local checks do NOT catch it — block it
// explicitly alongside the v4 link-local range that covers 169.254.169.254.
var blockedMetadataV6 = net.ParseIP("fd00:ec2::254")

// isBlockedEgressIP reports whether the UI must refuse to dial an address on
// behalf of a caller. The UI has no built-in auth (§3.1), so a caller can
// point a destination at an arbitrary host and use the differentiated
// test-connection / destination-stats errors as a reachability oracle — or
// aim at the cloud-metadata endpoint from the operator's network position.
//
// We block the ranges that are never a legitimate storage backend: loopback,
// link-local unicast (169.254.0.0/16 and fe80::/10 — this is the cloud
// metadata / IMDS range), the AWS IPv6 metadata address, unspecified, and
// multicast. RFC1918 / ULA private ranges are deliberately NOT blocked —
// in-cluster MinIO, a LAN NAS, or an internal SFTP host are the operator's
// core use case; constrain those further with a NetworkPolicy if needed.
func isBlockedEgressIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.Equal(blockedMetadataV6) {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// egressHost returns the user-controllable host from a destination config, or
// "" when there is nothing attacker-chosen to check. Only sftp/ftps `host`
// and a non-empty s3 `endpoint` are caller-supplied free-form hosts; azure and
// gcs derive their host from an account/bucket name under a fixed cloud
// domain, and an empty s3 endpoint means AWS S3's fixed endpoint.
func egressHost(d *secrets.Destination) string {
	switch d.StorageType {
	case "sftp", "hetzner-sftp", "ftps":
		return strings.TrimSpace(string(d.Data["host"]))
	case "s3":
		ep := strings.TrimSpace(string(d.Data["endpoint"]))
		if ep == "" {
			return ""
		}
		return hostFromEndpoint(ep)
	}
	return ""
}

// hostFromEndpoint extracts the bare host from an s3 endpoint that may be a
// plain host, host:port, or a full URL.
func hostFromEndpoint(ep string) string {
	if strings.Contains(ep, "://") {
		if u, err := url.Parse(ep); err == nil && u.Host != "" {
			ep = u.Host
		}
	}
	if h, _, err := net.SplitHostPort(ep); err == nil {
		return h
	}
	return ep
}

// checkDestinationEgress resolves the destination's user-controllable host and
// refuses if it (or any of its DNS answers) points at a blocked range. Applied
// at the single UI storage-construction choke point (storageForPool) so every
// UI-initiated dial — test-connection, destination-stats/health,
// consistency-check, download, dashboard probes — is covered in one place.
//
// TOCTOU note: a hostname can re-resolve to a blocked IP between this check
// and the actual dial (DNS rebinding). This guard raises the bar against the
// trivial "endpoint = 169.254.169.254 / metadata.google.internal" attack; it
// is defence-in-depth, not a replacement for a NetworkPolicy on the operator
// pod. On resolution failure we do NOT reject — the real dial surfaces the
// normal sanitized error rather than the guard inventing one.
func checkDestinationEgress(d *secrets.Destination) error {
	host := egressHost(d)
	if host == "" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedEgressIP(ip) {
			return fmt.Errorf("destination host %q is in a blocked address range", host)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if isBlockedEgressIP(ip) {
			return fmt.Errorf("destination host %q resolves to a blocked address range", host)
		}
	}
	return nil
}
