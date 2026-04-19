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
        refresh 60s
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
| `insecure_skip_verify` | no | accept self-signed PVE certs |
| `refresh` | no | inventory refresh interval (default `60s`) |
| `ttl` | no | A/AAAA record TTL (default `60`) |
| `fallthrough` | no | on no-match, hand off to the next plugin in the chain |

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
