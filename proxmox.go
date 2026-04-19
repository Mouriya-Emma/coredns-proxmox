// Package proxmox is a CoreDNS plugin that answers A/AAAA queries for
// Proxmox VM and LXC guest hostnames, resolving them via the Proxmox API.
//
// Key features:
//   - LXC discovery via /lxc/<id>/interfaces (no qemu-agent required)
//   - QEMU discovery via /qemu/<id>/agent/network-get-interfaces
//   - Per-IP CIDR allowlist: essential for SR-IOV passthrough VMs where
//     qemu-agent reports many non-LAN IPs (docker bridges, overlay meshes).
package proxmox

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

var log = clog.NewWithPlugin("proxmox")

// Proxmox is the CoreDNS plugin. One instance per server block.
type Proxmox struct {
	Next plugin.Handler

	// Zones this plugin is authoritative-ish for (derived from server block keys).
	// Names are normalised lowercase with no trailing dot.
	Zones []string

	// TTL for emitted A/AAAA records.
	TTL uint32

	// Refresh interval for the PVE inventory loop.
	Refresh time.Duration

	// AllowCIDRs, if non-empty, keeps only IPs in one of these prefixes.
	AllowCIDRs []netip.Prefix

	// ExcludeIPs drops these specific IPs from every emitted record. Matches
	// the scanner's IP_SKIP semantics — used to hide IPs already claimed by
	// an authoritative source (static hosts file, other PVE NICs).
	ExcludeIPs []netip.Addr

	// Fallthrough hands off to the next plugin on no-match.
	Fallthrough bool

	client pveapi.Client
	cancel context.CancelFunc

	// records holds the current name→IPs map. Swapped atomically on refresh.
	records atomic.Pointer[recordSet]
}

type recordSet map[string][]net.IP

func (p *Proxmox) Name() string { return "proxmox" }

func (p *Proxmox) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := strings.ToLower(state.Name())

	zone := plugin.Zones(p.Zones).Matches(strings.TrimSuffix(qname, "."))
	if zone == "" {
		return plugin.NextOrFailure(p.Name(), p.Next, ctx, w, r)
	}

	records := p.records.Load()
	if records == nil {
		return p.miss(ctx, w, r)
	}

	key := strings.TrimSuffix(qname, ".")
	ips, ok := (*records)[key]
	if !ok || len(ips) == 0 {
		return p.miss(ctx, w, r)
	}

	qtype := state.QType()
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil && qtype == dns.TypeA {
			msg.Answer = append(msg.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: state.Name(), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: p.TTL},
				A:   v4,
			})
		} else if v4 == nil && qtype == dns.TypeAAAA {
			msg.Answer = append(msg.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: state.Name(), Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: p.TTL},
				AAAA: ip,
			})
		}
	}

	if len(msg.Answer) == 0 {
		return p.miss(ctx, w, r)
	}

	if err := w.WriteMsg(msg); err != nil {
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}

func (p *Proxmox) miss(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	if p.Fallthrough {
		return plugin.NextOrFailure(p.Name(), p.Next, ctx, w, r)
	}
	// Authoritative no-data: empty answer section, NOERROR (matches hosts-plugin
	// behaviour when name is known but qtype doesn't match; NXDOMAIN would be
	// wrong since we only know a subset of the zone).
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	_ = w.WriteMsg(msg)
	return dns.RcodeSuccess, nil
}
