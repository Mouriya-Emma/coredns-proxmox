package proxmox

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

func init() {
	plugin.Register("proxmox", setup)
}

func setup(c *caddy.Controller) error {
	p, err := parse(c)
	if err != nil {
		return plugin.Error("proxmox", err)
	}

	c.OnStartup(p.Start)
	c.OnShutdown(p.Stop)

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		p.Next = next
		return p
	})
	return nil
}

func parse(c *caddy.Controller) (*Proxmox, error) {
	var (
		endpoint        string
		insecure        bool
		user            string
		tokenID         string
		tokenSecret     string
		tokenSecretFile string
		allowCIDRs      []netip.Prefix
		excludeIPs      []netip.Addr
		sriovStatePath  string
		permissive      bool
		reconcileEvery  = 60 * time.Second
		pollNever       = 60 * time.Second
		pollKnown       = 5 * time.Minute
		ttl             = uint32(60)
		fall            = false
	)

	zones := normaliseZones(c.ServerBlockKeys)

	for c.Next() {
		// proxmox directive accepts no positional args; config is all block subdirectives
		if args := c.RemainingArgs(); len(args) > 0 {
			return nil, c.Errf("proxmox: unexpected positional args %v", args)
		}
		for c.NextBlock() {
			switch c.Val() {
			case "endpoint":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				endpoint = c.Val()
			case "insecure_skip_verify":
				insecure = true
			case "user":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				user = c.Val()
			case "token_id":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				tokenID = c.Val()
			case "token_secret":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				tokenSecret = c.Val()
			case "token_secret_file":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				tokenSecretFile = c.Val()
			case "allow_cidr":
				args := c.RemainingArgs()
				if len(args) == 0 {
					return nil, c.ArgErr()
				}
				for _, s := range args {
					prefix, err := netip.ParsePrefix(s)
					if err != nil {
						return nil, c.Errf("invalid allow_cidr %q: %v", s, err)
					}
					allowCIDRs = append(allowCIDRs, prefix)
				}
			case "exclude_ip":
				args := c.RemainingArgs()
				if len(args) == 0 {
					return nil, c.ArgErr()
				}
				for _, s := range args {
					addr, err := netip.ParseAddr(s)
					if err != nil {
						return nil, c.Errf("invalid exclude_ip %q: %v", s, err)
					}
					excludeIPs = append(excludeIPs, addr.Unmap())
				}
			case "sriov_state":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				sriovStatePath = c.Val()
			case "permissive":
				// Opt-in. With no arg, turns the permissive channel on with
				// default drop-lists (docker*, br-*, veth*, cni-*, lo, wt0).
				// Future: may accept override flags here.
				permissive = true
				if args := c.RemainingArgs(); len(args) > 0 {
					return nil, c.Errf("permissive takes no args (got %v)", args)
				}
			case "refresh":
				// Back-compat alias for reconcile_every: old Corefiles may
				// still set `refresh`. New three-knob form is preferred.
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid refresh %q: %v", c.Val(), err)
				}
				reconcileEvery = d
			case "reconcile_every":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid reconcile_every %q: %v", c.Val(), err)
				}
				reconcileEvery = d
			case "poll_never_ips":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid poll_never_ips %q: %v", c.Val(), err)
				}
				pollNever = d
			case "poll_known_ips":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid poll_known_ips %q: %v", c.Val(), err)
				}
				pollKnown = d
			case "ttl":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				n, err := strconv.ParseUint(c.Val(), 10, 32)
				if err != nil {
					return nil, c.Errf("invalid ttl %q: %v", c.Val(), err)
				}
				ttl = uint32(n)
			case "fallthrough":
				fall = true
			default:
				return nil, c.Errf("unknown directive %q", c.Val())
			}
		}
	}

	if tokenSecretFile != "" {
		data, err := os.ReadFile(tokenSecretFile)
		if err != nil {
			return nil, fmt.Errorf("reading token_secret_file %q: %w", tokenSecretFile, err)
		}
		tokenSecret = strings.TrimSpace(string(data))
	}
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint required")
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	if user == "" {
		return nil, fmt.Errorf("user required")
	}
	if tokenID == "" {
		return nil, fmt.Errorf("token_id required")
	}
	if tokenSecret == "" {
		return nil, fmt.Errorf("token_secret or token_secret_file required")
	}

	httpc := &http.Client{Timeout: 30 * time.Second}
	if insecure {
		httpc.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in for self-signed PVE certs
		}
	}

	auth := &pveapi.APITokenAuthProvider{
		User:    user,
		TokenID: tokenID,
		Secret:  tokenSecret,
	}
	client := pveapi.NewClient(httpc, strings.TrimRight(endpoint, "/"), auth)

	return &Proxmox{
		Zones:             zones,
		TTL:               ttl,
		ReconcileEvery:    reconcileEvery,
		PollNever:         pollNever,
		PollKnown:         pollKnown,
		AllowCIDRs:        allowCIDRs,
		ExcludeIPs:        excludeIPs,
		SriovStatePath:    sriovStatePath,
		PermissiveChannel: permissive,
		Fallthrough:       fall,
		client:            client,
	}, nil
}

func normaliseZones(raw []string) []string {
	// Server-block keys can look like "dns://hb.lan.:5300", "hb.lan:5300",
	// "tls://hb.lan", etc. plugin.Zones.Matches expects DNS-canonical form
	// (lowercase, trailing dot), which is also what we key the record map by
	// and what client qnames carry. Strip protocol prefix + :port.
	out := make([]string, 0, len(raw))
	for _, z := range raw {
		if i := strings.Index(z, "://"); i >= 0 {
			z = z[i+3:]
		}
		if i := strings.LastIndex(z, ":"); i >= 0 {
			z = z[:i]
		}
		z = strings.ToLower(strings.TrimSpace(z))
		if z == "" {
			continue
		}
		if !strings.HasSuffix(z, ".") {
			z += "."
		}
		out = append(out, z)
	}
	return out
}
