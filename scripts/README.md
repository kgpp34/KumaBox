# Host networking helpers

KumaBox's default CNI networking needs one host-level MASQUERADE rule; the
optional direct-bridge mode needs a bridge and (by default) DHCP. These scripts
make both paths reproducible.

## `kumabox-check.sh`

```bash
sudo kumabox-check.sh              # read-only checks
sudo kumabox-check.sh --fix        # fix dirs/sysctl/iptables/CNI config
sudo kumabox-check.sh --upgrade    # also install CH/CNI dependencies
```

`--fix` generates a CNI conflist with `"ipMasq": false` and installs
`kumabox-masquerade.service` plus one host rule:

```text
ip saddr <sandbox-subnet> ip daddr != <sandbox-subnet> masquerade
```

This replaces the CNI bridge plugin's per-sandbox masquerade rules. Drain old
sandboxes before switching an existing host, and clean any leftover
`inet cni_plugins_masquerade` table manually.

## `kumabox-bridge.sh`

```bash
# DHCP (default, matches Cocoon's bridge mode)
sudo kumabox-bridge.sh create kubebr0 10.99.0.0/16

# Static IPAM (no DHCP server)
sudo kumabox-bridge.sh create kubebr0 10.99.0.0/16 --static

sudo kumabox-bridge.sh delete kubebr0
```

The DHCP path installs and starts
`kumabox-bridge-dhcp@<bridge>.service` (environment in
`/etc/kumabox/bridge-<bridge>.env`) so dnsmasq survives a reboot. The script
does not persist the bridge device itself; use systemd-networkd or your normal
network configuration for that.

Then create a sandbox with:

```bash
sudo kumabox run IMAGE --bridge kubebr0 --nics 1
# or, without a DHCP server:
sudo kumabox run IMAGE --bridge kubebr0 --bridge-static --nics 1
```

Bridge TAPs are named `bt<id8>-<index>`. DHCP is the default; `--bridge-static`
uses KumaBox's deterministic lease directory under
`/var/lib/kumabox/network/bridge/<bridge>/`.

## Live resize

Both CNI and bridge modes support:

```bash
sudo kumabox net SANDBOX --nics N
```

DHCP bridge resize relies on the guest's `systemd-networkd`; `--wait-network`
uses the current guest agent's DHCP reconfiguration path.
