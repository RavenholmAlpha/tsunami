package server

import (
	"net"
)

var privateNets = []net.IPNet{
	{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)},
	{IP: net.IPv4(10, 0, 0, 0), Mask: net.CIDRMask(8, 32)},
	{IP: net.IPv4(172, 16, 0, 0), Mask: net.CIDRMask(12, 32)},
	{IP: net.IPv4(192, 168, 0, 0), Mask: net.CIDRMask(16, 32)},
	{IP: net.IPv4(169, 254, 0, 0), Mask: net.CIDRMask(16, 32)},
	{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(32, 32)},
	parseCIDR("::1/128"),
	parseCIDR("fc00::/7"),
	parseCIDR("fe80::/10"),
}

func parseCIDR(s string) net.IPNet {
	_, n, _ := net.ParseCIDR(s)
	return *n
}

func isPrivateIP(ip net.IP) bool {
	for i := range privateNets {
		if privateNets[i].Contains(ip) {
			return true
		}
	}
	return false
}

// isPrivateTarget checks whether the target host resolves to a private/reserved IP.
// For IP literals it checks directly; for domains it resolves first.
func isPrivateTarget(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}

	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}
