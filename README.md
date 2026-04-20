# coredns-proxmox

CoreDNS plugin that resolves Proxmox VM and LXC guest hostnames by querying the
Proxmox API. Designed for homelab setups where:

- Some VMs use SR-IOV passthrough and therefore never appear on the hypervisor
  bridge (ARP/mDNS scanners can't see them).
- Every guest's only authoritative IP source is `qemu-guest-agent` / LXC
  runtime interface list.
- The resolver must filter out non-LAN IPs (docker bridges, netbird meshes,
  IPv6 link-local) that `network-get-interfaces` happily returns.

## Corefile

```
hb.lan:5300 {
    proxmox {
        endpoint https://pve.hb.lan:8006
        insecure_skip_verify
        user root@pam
        token_id coredns-proxmox
        token_secret_file /etc/coredns/pve-token
        allow_cidr 192.168.1.0/24
        exclude_ip 192.168.1.22 192.168.1.67
        reconcile_every 60s
        poll_never_ips 60s
        poll_known_ips 5m
        ttl 60
        fallthrough
    }
    hosts /etc/coredns/lan.hosts hb.lan {
        fallthrough
    }
    log
    errors
    cache 30
}
```

Directive reference:

| key | required | description |
| --- | --- | --- |
| `endpoint` | yes | PVE API base URL (e.g. `https://pve:8006`) |
| `user` | yes | API user, e.g. `root@pam` |
| `token_id` | yes | API token ID (the part after `!`) |
| `token_secret` | one of | the raw token secret (keep secrets out of Corefile) |
| `token_secret_file` | one of | path to a file containing the secret |
| `allow_cidr` | no | one or more CIDRs; if any given, only IPs in one of these CIDRs are returned. Repeatable; multiple values on one line allowed |
| `exclude_ip` | no | drop these specific IPs from every emitted record. Use for IPs already claimed by a static source (e.g. authoritative host file, hypervisor secondary NICs). Repeatable; multiple values on one line allowed |
| `sriov_state` | no | path to a JSON file produced by `sriov dump` (run on the PVE host, shipped to the plugin via a bind mount). When present, the plugin filters per-guest interfaces by hardware MAC against the SR-IOV VF adminMacs the guest owns. More precise than `allow_cidr` — MAC identity is exact whereas CIDRs can match unrelated bridges that happen to share the subnet |
| `insecure_skip_verify` | no | accept self-signed PVE certs |
| `reconcile_every` | no | how often the supervisor re-enumerates the cluster to spawn/cancel per-guest goroutines. Default `60s`. `refresh` is accepted as a back-compat alias |
| `poll_never_ips` | no | per-guest poll cadence *until that guest first returns IPs*. Default `60s` — keep this short so slow-booting VMs show up in DNS soon after agent becomes responsive |
| `poll_known_ips` | no | per-guest poll cadence *after* first success. Default `5m` — long enough to avoid hammering PVE, short enough to catch an IP change within minutes |
| `ttl` | no | A/AAAA record TTL (default `60`) |
| `fallthrough` | no | on no-match, hand off to the next plugin in the chain |

## SR-IOV MAC filtering (optional, since v0.1.3)

When `sriov_state` points at a JSON dump produced by the [`sriov`
CLI](https://github.com/andrew-d/proxmox-service-discovery) (`sriov dump`),
the plugin learns which hardware MAC belongs to each guest's SR-IOV VF(s)
and uses it to filter the qemu-agent / lxc interface response.

Producer side (on the PVE host):

```
# systemd timer, every 30 s or so
sriov dump > /var/lib/sriov-state/dump.json.tmp \
  && mv /var/lib/sriov-state/dump.json.tmp /var/lib/sriov-state/dump.json
```

Consumer side (the plugin): the dump file is read-only bind-mounted into
the CT that runs CoreDNS; `sriov_state /var/lib/sriov-state/dump.json` in
the Corefile. The plugin re-reads on mtime change, so the timer rewrites
pick up without restart.

Schema consumed: `correlatedVfs[].vf.{kind, adminMac}` and
`correlatedVfs[].consumers[]` with kinds `vm-direct`, `vm-via-mapping`
(multiple `vmids`), and `container-phys`. Everything else is ignored —
GPU VFs (no MAC), host-owned VFs, unused VFs.

Behaviour per interface at poll time (**additive**, not exclusive):

- Interface's MAC matches one of the guest's SR-IOV adminMacs → keep.
  Authoritative include: this is a confirmed VF belonging to the guest,
  regardless of the ifname (defensive against unusual names).
- Otherwise → name heuristic. Drop known-noise prefixes (`lo`, `docker*`,
  `br-*`, `veth*`, `cni-*`, `wt0`). Keep everything else (`ethN`,
  `enpNsN`, `net0`, etc.).

This combines channels rather than replacing them. A guest with an
SR-IOV VF plus a regular `net0` on a vmbr will have **both**
interfaces' IPs surfaced. An earlier version of this plugin gated
solely on MAC when `sriov_state` was set, silently dropping the
vmbr-side NIC — that was wrong: SR-IOV is one discovery channel, not
an exclusive filter.

`allow_cidr` / `exclude_ip` still apply *after* this on the IPs
themselves, so an operator can trim IPv6 globals, unrelated bridge
ranges, etc. at the address level.

## Cold-start + slow-boot design

Each PVE guest gets its own discovery goroutine. Why:

- A VM can take minutes from power-on to qemu-agent responding. If discovery
  ran as a single "refresh the whole cluster every N seconds" loop, a
  still-booting VM would be repeatedly absent from the records map, and a
  transient agent failure on any one guest would evict its record even
  though we had a fresh answer seconds ago.
- With per-guest goroutines, each guest progresses on its own schedule.
  Fast-boot LXCs populate immediately; slow-boot VMs appear when their
  agent catches up. Nothing blocks anything else.

Cadence is two-level:

- `poll_never_ips` (aggressive, default 60s) runs until the guest first
  returns usable IPs. This is the "still warming up" poll — we want DNS
  to start resolving the guest as soon as it's ready.
- `poll_known_ips` (relaxed, default 5m) takes over after the first
  success. The only reason to keep polling is to catch an IP change, so
  this can be slow. A per-poll jitter of ±10% prevents all goroutines
  from synchronising.

Records for a given guest are only *evicted* when the cluster-list
enumeration confirms the guest is gone (destroyed) or no longer running
(stopped/paused). A failed agent call — or agent returning empty IPs —
never evicts; it just means "try again next tick."

Plugin chain order matters: place `proxmox` **before** `hosts` in the Corefile
and in `plugin.cfg` so PVE-authoritative names win over scanner-derived entries
for the same hostname.

## Build

```
make build
```

Clones `coredns v1.14.2`, inserts the plugin into `plugin.cfg` above the `hosts`
entry, builds a binary at `./coredns`. `make all` also runs the smoke test.

Override CoreDNS version: `make build COREDNS_VERSION=1.14.1`.

## PVE API token

Minimum privileges (verified live: removing any one fails the corresponding endpoint):

| privilege | endpoint(s) that require it |
| --- | --- |
| `Sys.Audit` | `/nodes` |
| `VM.Audit`  | `/nodes/<n>/qemu`, `/nodes/<n>/lxc`, `/nodes/<n>/lxc/<id>/interfaces` |
| `VM.Monitor` | `/nodes/<n>/qemu/<id>/agent/network-get-interfaces` (qemu-agent commands) |

Setup:

```
pveum role add DNSDiscovery --privs "Sys.Audit,VM.Audit,VM.Monitor"
pveum user token add root@pam coredns-proxmox --privsep 1   # prints the secret
pveum acl modify / --roles DNSDiscovery --tokens 'root@pam!coredns-proxmox' --propagate 1
```

Capture the secret printed by `token add` and write it (only) to the file
referenced by `token_secret_file`, mode `0600` owned by the coredns user.

## Licensing & attribution

Apache License 2.0 — see [`LICENSE`](LICENSE).

`internal/pveapi/` is adapted from
[`andrew-d/proxmox-service-discovery`](https://github.com/andrew-d/proxmox-service-discovery)'s
`internal/pveapi` package (Apache 2.0). Only the HTTP client, token auth, and
response types were vendored; the rest of this plugin is new code.
