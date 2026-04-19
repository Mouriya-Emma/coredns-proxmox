package proxmox

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Start kicks off the discovery loop. Returns after the first refresh attempt;
// an initial failure is logged but does not block startup (fallthrough is still
// valid).
func (p *Proxmox) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	if err := p.doRefresh(ctx); err != nil {
		log.Warningf("initial PVE inventory refresh failed: %v", err)
	}

	go p.loop(ctx)
	return nil
}

// Stop halts the discovery loop.
func (p *Proxmox) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

func (p *Proxmox) loop(ctx context.Context) {
	t := time.NewTicker(p.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.doRefresh(ctx); err != nil {
				log.Warningf("PVE inventory refresh failed: %v", err)
			}
		}
	}
}

func (p *Proxmox) doRefresh(ctx context.Context) error {
	rc, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	records, err := p.discover(rc)
	if err != nil {
		return err
	}
	p.records.Store(&records)
	log.Debugf("refreshed inventory: %d names", len(records))
	return nil
}

func (p *Proxmox) discover(ctx context.Context) (recordSet, error) {
	nodes, err := p.client.GetNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	out := make(recordSet)
	for _, node := range nodes {
		p.discoverLXC(ctx, out, node.Node)
		p.discoverQEMU(ctx, out, node.Node)
	}
	return out, nil
}

func (p *Proxmox) discoverLXC(ctx context.Context, out recordSet, node string) {
	lxcs, err := p.client.GetLXCs(ctx, node)
	if err != nil {
		log.Warningf("list LXCs on node %s: %v", node, err)
		return
	}
	for _, lxc := range lxcs {
		if lxc.Status != "running" {
			continue
		}
		ips := p.lxcIPs(ctx, node, lxc.VMID)
		p.addRecords(out, lxc.Name, ips)
	}
}

func (p *Proxmox) discoverQEMU(ctx context.Context, out recordSet, node string) {
	vms, err := p.client.GetQEMUVMs(ctx, node)
	if err != nil {
		log.Warningf("list QEMUs on node %s: %v", node, err)
		return
	}
	for _, vm := range vms {
		if vm.Status != "running" {
			continue
		}
		ips := p.qemuIPs(ctx, node, vm.VMID)
		p.addRecords(out, vm.Name, ips)
	}
}

func (p *Proxmox) lxcIPs(ctx context.Context, node string, vmid int) []net.IP {
	ifs, err := p.client.GetLXCInterfaces(ctx, node, vmid)
	if err != nil {
		log.Debugf("lxc %d/%s interfaces: %v", vmid, node, err)
		return nil
	}
	var ips []net.IP
	for _, ifc := range ifs {
		if ifc.Name == "lo" {
			continue
		}
		for _, raw := range []string{ifc.Inet, ifc.Inet6} {
			ips = appendParsed(ips, raw)
		}
	}
	return p.filterAllowed(ips)
}

func (p *Proxmox) qemuIPs(ctx context.Context, node string, vmid int) []net.IP {
	resp, err := p.client.GetQEMUInterfaces(ctx, node, vmid)
	if err != nil {
		// Very common: VM without qemu-guest-agent → 500.
		log.Debugf("qemu %d/%s interfaces: %v", vmid, node, err)
		return nil
	}
	var ips []net.IP
	for _, ifc := range resp.Result {
		if ifc.Name == "lo" || strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") {
			continue
		}
		for _, a := range ifc.IPAddresses {
			ips = appendParsed(ips, a.Address)
		}
	}
	return p.filterAllowed(ips)
}

func appendParsed(ips []net.IP, raw string) []net.IP {
	if raw == "" {
		return ips
	}
	if i := strings.Index(raw, "/"); i >= 0 {
		raw = raw[:i]
	}
	if ip := net.ParseIP(raw); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() {
		ips = append(ips, ip)
	}
	return ips
}

func (p *Proxmox) filterAllowed(ips []net.IP) []net.IP {
	if len(p.AllowCIDRs) == 0 {
		return ips
	}
	var keep []net.IP
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		a = a.Unmap()
		for _, cidr := range p.AllowCIDRs {
			if cidr.Contains(a) {
				keep = append(keep, ip)
				break
			}
		}
	}
	return keep
}

func (p *Proxmox) addRecords(out recordSet, name string, ips []net.IP) {
	if name == "" || len(ips) == 0 {
		return
	}
	// Strip any trailing dot and normalise to lowercase short-name.
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	// If the PVE hostname already looks FQDN-ish (contains a dot), take the leaf.
	if i := strings.Index(name, "."); i >= 0 {
		name = name[:i]
	}
	for _, zone := range p.Zones {
		fqdn := name + "." + zone
		out[fqdn] = append(out[fqdn], ips...)
	}
}
