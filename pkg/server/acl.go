package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	targetDNSCacheTTL        = 30 * time.Second
	maxTargetDNSCacheEntries = 2048
)

type cachedDNSResult struct {
	addrs      []netip.Addr
	expireAtNs int64
}

type dnsInflightCall struct {
	done  chan struct{}
	addrs []netip.Addr
	err   error
}

type targetDNSCache struct {
	mu       sync.RWMutex
	entries  map[string]cachedDNSResult
	inflight map[string]*dnsInflightCall
}

var globalTargetDNSCache = &targetDNSCache{
	entries:  make(map[string]cachedDNSResult),
	inflight: make(map[string]*dnsInflightCall),
}

func (c *targetDNSCache) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	nowNs := time.Now().UnixNano()
	c.mu.RLock()
	if cached, ok := c.entries[host]; ok && nowNs < cached.expireAtNs {
		c.mu.RUnlock()
		return cached.addrs, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	if cached, ok := c.entries[host]; ok {
		if nowNs < cached.expireAtNs {
			c.mu.Unlock()
			return cached.addrs, nil
		}
		delete(c.entries, host)
	}
	if call, ok := c.inflight[host]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.addrs, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &dnsInflightCall{done: make(chan struct{})}
	c.inflight[host] = call
	c.mu.Unlock()

	lookupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	addrs, err := net.DefaultResolver.LookupNetIP(lookupCtx, "ip", host)
	cancel()

	if err != nil {
		call.err = fmt.Errorf("resolve %q: %w", host, err)
	} else if len(addrs) == 0 {
		call.err = fmt.Errorf("resolve %q: no addresses", host)
	} else {
		res := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			res = append(res, a.Unmap())
		}
		call.addrs = res
	}

	c.mu.Lock()
	delete(c.inflight, host)
	if call.err == nil && len(call.addrs) > 0 {
		expireNs := time.Now().Add(targetDNSCacheTTL).UnixNano()
		if len(c.entries) >= maxTargetDNSCacheEntries {
			curNs := time.Now().UnixNano()
			for k, v := range c.entries {
				if curNs >= v.expireAtNs {
					delete(c.entries, k)
				}
			}
			if len(c.entries) >= maxTargetDNSCacheEntries {
				for k := range c.entries {
					delete(c.entries, k)
					break
				}
			}
		}
		c.entries[host] = cachedDNSResult{addrs: call.addrs, expireAtNs: expireNs}
	}
	close(call.done)
	c.mu.Unlock()

	if ctxErr := ctx.Err(); ctxErr != nil && call.err != nil {
		return nil, ctxErr
	}
	return call.addrs, call.err
}

// errBlockedTarget is returned when a destination is rejected by the
// private/reserved address policy.
var errBlockedTarget = errors.New("tsunami: destination blocked by private-address policy")

// privatePrefixes covers loopback, RFC1918, link-local, CGNAT, benchmarking,
// documentation, multicast and reserved space. CGNAT matters in practice
// because several cloud metadata services live in 100.64.0.0/10.
var privatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("ff00::/8"),
}

// isPrivateAddr reports whether addr is private, reserved, link-local, or
// carries an IPv6 zone (zones can address link-local scopes by interface).
func isPrivateAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap()
	if addr.Zone() != "" {
		return true
	}
	for _, prefix := range privatePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return isPrivateAddr(addr)
}

// isPrivateTarget checks whether the target host resolves to a private or
// reserved address. It fails closed on unparseable targets and DNS errors so a
// later re-resolution cannot downgrade the decision.
func isPrivateTarget(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return true
	}
	return isPrivateHost(host)
}

// isPrivateHost validates a host without a port. IP literals are checked
// directly; domains are resolved once and every returned address must be
// public. Failures are treated as blocked.
func isPrivateHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return true
	}
	if strings.Contains(host, "%") {
		// IPv6 zone identifiers address link-local/interface-scoped endpoints.
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return isPrivateAddr(addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return true
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}

// splitTarget parses a "host:port" target and validates the port. IPv6 zone
// identifiers are rejected.
func splitTarget(target string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(target)
	if err != nil {
		return "", "", fmt.Errorf("tsunami: invalid target %q: %w", target, err)
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", "", errBlockedTarget
	}
	if strings.Contains(host, "%") {
		return "", "", errBlockedTarget
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", "", fmt.Errorf("tsunami: invalid target port %q", port)
	}
	return host, port, nil
}

// resolveTargetIPs resolves host using a bounded short-TTL cache with concurrent
// singleflight deduplication. IP literals do not hit the resolver or cache.
func resolveTargetIPs(ctx context.Context, host string) ([]netip.Addr, error) {
	key := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if addr, err := netip.ParseAddr(key); err == nil {
		if addr.Zone() != "" {
			return nil, errBlockedTarget
		}
		return []netip.Addr{addr.Unmap()}, nil
	}
	return globalTargetDNSCache.lookup(ctx, key)
}

const (
	targetDialTimeout  = 10 * time.Second
	happyEyeballsDelay = 250 * time.Millisecond
)

// dialTarget connects to target while enforcing the private-address policy.
//
// The address is resolved exactly once, every returned address is validated,
// and the connection is made to the validated IP (not the original hostname),
// so a DNS-rebinding domain cannot pass the check as public and then resolve to
// an internal address inside net.Dial.
func (s *Server) dialTarget(ctx context.Context, target string) (net.Conn, error) {
	if s.config.AllowAll {
		host, port, err := splitTarget(target)
		if err != nil {
			dialer := &net.Dialer{
				Timeout:       targetDialTimeout,
				KeepAlive:     30 * time.Second,
				FallbackDelay: happyEyeballsDelay,
			}
			return dialer.DialContext(ctx, "tcp", target)
		}
		addresses, err := resolveTargetIPs(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addresses) == 1 {
			dialer := &net.Dialer{Timeout: targetDialTimeout, KeepAlive: 30 * time.Second}
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(addresses[0].String(), port))
		}
		return dialHappyEyeballs(ctx, interleaveAddresses(addresses), port, happyEyeballsDelay, targetDialTimeout)
	}
	host, port, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	addresses, err := resolveTargetIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, addr := range addresses {
		if isPrivateAddr(addr) {
			return nil, errBlockedTarget
		}
	}
	if len(addresses) == 1 {
		dialer := &net.Dialer{Timeout: targetDialTimeout, KeepAlive: 30 * time.Second}
		return dialer.DialContext(ctx, "tcp", net.JoinHostPort(addresses[0].String(), port))
	}
	return dialHappyEyeballs(ctx, interleaveAddresses(addresses), port, happyEyeballsDelay, targetDialTimeout)
}

// interleaveAddresses alternates IPv4 and IPv6 candidates starting with IPv4
// when both families are available, avoiding 250ms Happy Eyeballs stalls on
// IPv4-only VPS/NAT servers whose upstream DNS still returns AAAA records.
func interleaveAddresses(addrs []netip.Addr) []netip.Addr {
	if len(addrs) <= 1 {
		return addrs
	}
	var v4, v6 []netip.Addr
	for _, addr := range addrs {
		if addr.Is4() {
			v4 = append(v4, addr)
		} else {
			v6 = append(v6, addr)
		}
	}
	if len(v4) == 0 || len(v6) == 0 {
		return addrs
	}
	out := make([]netip.Addr, 0, len(addrs))
	for i := 0; i < len(v4) || i < len(v6); i++ {
		if i < len(v4) {
			out = append(out, v4[i])
		}
		if i < len(v6) {
			out = append(out, v6[i])
		}
	}
	return out
}

func dialHappyEyeballs(ctx context.Context, addrs []netip.Addr, port string, staggerDelay, timeout time.Duration) (net.Conn, error) {
	if len(addrs) == 0 {
		return nil, errors.New("no resolved addresses")
	}

	type attemptResult struct {
		conn net.Conn
		err  error
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make(chan attemptResult, len(addrs))
	dialOne := func(addr netip.Addr) {
		dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
		conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(addr.String(), port))
		results <- attemptResult{conn: conn, err: err}
	}

	go dialOne(addrs[0])
	started := 1
	failures := 0
	var lastErr error

	timer := time.NewTimer(staggerDelay)
	defer timer.Stop()

	for failures < len(addrs) {
		var timerC <-chan time.Time
		if started < len(addrs) {
			timerC = timer.C
		}
		select {
		case res := <-results:
			if res.err == nil {
				cancel()
				if remaining := started - failures - 1; remaining > 0 {
					// Drain already-started attempts in the background so any late connection is closed.
					go func(rem int) {
						for i := 0; i < rem; i++ {
							r := <-results
							if r.conn != nil {
								_ = r.conn.Close()
							}
						}
					}(remaining)
				}
				return res.conn, nil
			}
			failures++
			lastErr = res.err
			if started < len(addrs) {
				// Launch the next candidate immediately instead of waiting for stagger.
				go dialOne(addrs[started])
				started++
				if started < len(addrs) {
					timer.Reset(staggerDelay)
				}
			}
		case <-timerC:
			if started < len(addrs) {
				go dialOne(addrs[started])
				started++
				if started < len(addrs) {
					timer.Reset(staggerDelay)
				}
			}
		case <-dialCtx.Done():
			if remaining := started - failures; remaining > 0 {
				go func(rem int) {
					for i := 0; i < rem; i++ {
						r := <-results
						if r.conn != nil {
							_ = r.conn.Close()
						}
					}
				}(remaining)
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, dialCtx.Err()
		}
	}
	return nil, lastErr
}

// resolveUDPTarget resolves and validates a UoT target once, using the shared
// DNS cache even when allowAll is set to avoid uncached OS DNS lookups.
func resolveUDPTarget(ctx context.Context, target string, allowAll bool) (*net.UDPAddr, error) {
	host, portText, err := splitTarget(target)
	if err != nil {
		if allowAll {
			return net.ResolveUDPAddr("udp", target)
		}
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("tsunami: invalid UDP target port %q", portText)
	}
	addresses, err := resolveTargetIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	if !allowAll {
		for _, addr := range addresses {
			if isPrivateAddr(addr) {
				return nil, errBlockedTarget
			}
		}
	}
	ordered := interleaveAddresses(addresses)
	return &net.UDPAddr{IP: net.IP(ordered[0].AsSlice()), Port: port}, nil
}
