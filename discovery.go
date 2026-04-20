package proxmox

import (
	"net"
	"net/netip"
	"strings"
)

// appendParsed parses a raw IP / CIDR-IP string (e.g. "192.168.1.22" or
// "192.168.1.22/24") and appends it to ips if it's a sensible address —
// dropping loopback, link-local and unspecified.
func appendParsed(ips []net.IP, raw string) []net.IP {
	if raw == "" {
		return ips
	}
	if i := strings.Index(raw, "/"); i >= 0 {
		raw = raw[:i]
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return ips
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ips
	}
	return append(ips, ip)
}

// filterIPs applies allow_cidr (keep) then exclude_ip (drop). With no
// allowCIDRs, everything is allowed. exclude_ip takes precedence — an IP in
// both lists is dropped.
func filterIPs(ips []net.IP, allow []netip.Prefix, exclude []netip.Addr) []net.IP {
	if len(allow) == 0 && len(exclude) == 0 {
		return ips
	}
	var out []net.IP
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		a = a.Unmap()
		if isExcluded(a, exclude) {
			continue
		}
		if !isAllowed(a, allow) {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func isAllowed(a netip.Addr, allow []netip.Prefix) bool {
	if len(allow) == 0 {
		return true
	}
	for _, cidr := range allow {
		if cidr.Contains(a) {
			return true
		}
	}
	return false
}

func isExcluded(a netip.Addr, exclude []netip.Addr) bool {
	for _, ex := range exclude {
		if ex == a {
			return true
		}
	}
	return false
}
