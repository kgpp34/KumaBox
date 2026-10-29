// SPDX-License-Identifier: MIT
//! Parse the existing overlay-v1 kernel arguments before any disk is mounted.

use std::collections::HashSet;
use std::net::Ipv4Addr;
use std::time::Duration;

#[derive(Debug, PartialEq, Eq)]
pub struct Network {
    /// Guest address passed by the host boot contract.
    pub address: Ipv4Addr,
    /// Omitted for a link without a default route.
    pub gateway: Option<Ipv4Addr>,
    /// Prefix length validated from a contiguous IPv4 netmask.
    pub prefix: u32,
    /// Early kernel name; persisted network rules match its MAC instead.
    pub interface: String,
    /// Up to two host-selected DNS servers.
    pub dns: Vec<Ipv4Addr>,
}

#[derive(Debug)]
pub struct Config {
    /// Immutable disk serials ordered from highest to lowest overlay priority.
    pub layers: Vec<String>,
    /// Private ext4 disk holding the persistent upper and work directories.
    pub cow: String,
    /// Sandbox identity installed before the real init can override it.
    pub hostname: String,
    /// One wait budget shared by the entire block-device set.
    pub timeout: Duration,
    /// Static NIC settings; empty leaves a zero-NIC guest untouched.
    pub networks: Vec<Network>,
    /// Emit one deferred line of cumulative startup timings.
    pub trace: bool,
}

impl Config {
    pub fn parse(command: &str) -> Result<Self, String> {
        let mut cfg = Self {
            layers: Vec::new(),
            cow: String::new(),
            hostname: String::new(),
            timeout: Duration::from_secs(10),
            networks: Vec::new(),
            trace: false,
        };
        for token in command.split_ascii_whitespace() {
            let (key, value) = token.split_once('=').unwrap_or((token, ""));
            match key {
                "kumabox.layers" => cfg.layers = value.split(',').map(str::to_owned).collect(),
                "kumabox.cow" => cfg.cow = value.to_owned(),
                "kumabox.hostname" => cfg.hostname = value.to_owned(),
                "kumabox.timeout" => {
                    let seconds: u64 = value.parse().map_err(|_| "invalid device timeout")?;
                    if !(1..=120).contains(&seconds) {
                        return Err("device timeout must be 1..120 seconds".into());
                    }
                    cfg.timeout = Duration::from_secs(seconds);
                }
                "kumabox.trace" => cfg.trace = value.is_empty() || value == "1",
                "ip" if !matches!(value, "off" | "none" | "dhcp") => {
                    cfg.networks.push(Network::parse(value)?)
                }
                _ => {}
            }
        }
        if cfg.layers.is_empty() || cfg.layers.len() > 128 {
            return Err("expected 1..128 root layers".into());
        }
        let mut serials = HashSet::new();
        for serial in cfg.layers.iter().chain([&cfg.cow]) {
            if !safe_component(serial, 255) || !serials.insert(serial) {
                return Err("invalid or duplicate disk serial".into());
            }
        }
        if !safe_component(&cfg.hostname, 63) {
            return Err("missing or invalid hostname".into());
        }
        let mut interfaces = HashSet::new();
        for nic in &cfg.networks {
            if !interfaces.insert(&nic.interface) {
                return Err("duplicate network interface".into());
            }
        }
        Ok(cfg)
    }
}

pub fn debug_requested(command: &str) -> bool {
    command
        .split_ascii_whitespace()
        .filter_map(|token| {
            let (key, value) = token.split_once('=').unwrap_or((token, ""));
            (key == "kumabox.debug").then_some(value.is_empty() || value == "1")
        })
        .next_back()
        .unwrap_or(false)
}

fn safe_component(value: &str, limit: usize) -> bool {
    !value.is_empty()
        && value.len() <= limit
        && value != "."
        && value != ".."
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'-'))
}

impl Network {
    fn parse(value: &str) -> Result<Self, String> {
        let fields: Vec<_> = value.split(':').collect();
        if !(7..=9).contains(&fields.len()) || !safe_component(fields[5], 15) {
            return Err("invalid static network argument".into());
        }
        let address: Ipv4Addr = fields[0]
            .parse()
            .map_err(|_| "invalid guest IPv4 address")?;
        let mask: Ipv4Addr = fields[3].parse().map_err(|_| "invalid IPv4 netmask")?;
        let bits = u32::from(mask);
        let prefix = bits.leading_ones();
        if bits != u32::MAX.checked_shl(32 - prefix).unwrap_or(0) {
            return Err("IPv4 netmask is not contiguous".into());
        }
        let optional_ip = |s: &str| -> Result<Option<Ipv4Addr>, String> {
            if s.is_empty() || s == "0.0.0.0" {
                return Ok(None);
            }
            s.parse()
                .map(Some)
                .map_err(|_| "invalid gateway or DNS address".into())
        };
        let mut dns = Vec::new();
        for field in &fields[7..] {
            if let Some(ip) = optional_ip(field)? {
                dns.push(ip);
            }
        }
        Ok(Self {
            address,
            gateway: optional_ip(fields[2])?,
            prefix,
            interface: fields[5].to_owned(),
            dns,
        })
    }

    // Fixed link-local identities need explicit global scope and source routes.
    // Keep this contract identical to the host eBPF provider and guest agent.
    pub fn unit(&self, mac: &str) -> String {
        let mut out = format!("[Match]\nMACAddress={mac}\n\n[Network]\n");
        let fixed = self.address.is_link_local();
        if fixed {
            out.push_str("DHCP=no\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n");
        } else {
            out.push_str(&format!("Address={}/{}\n", self.address, self.prefix));
            if let Some(gateway) = self.gateway {
                out.push_str(&format!("Gateway={gateway}\n"));
            }
        }
        for ip in &self.dns {
            out.push_str(&format!("DNS={ip}\n"));
        }
        if fixed {
            if self.gateway.is_some() {
                out.push_str("DNSDefaultRoute=yes\n");
            }
            out.push_str(&format!(
                "\n[Address]\nAddress={}/{}\nScope=global\nDuplicateAddressDetection=none\n",
                self.address, self.prefix
            ));
            if let Some(gateway) = self.gateway {
                out.push_str(&format!(
                    "\n[Route]\nDestination=0.0.0.0/0\nGateway={gateway}\nPreferredSource={}\n",
                    self.address
                ));
            }
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    const BASE: &str = "kumabox.layers=layer1,layer0 kumabox.cow=cow kumabox.hostname=guest";

    #[test]
    fn validates_disk_contract_and_bounds_waits() {
        let config = Config::parse(BASE).unwrap();
        assert_eq!(config.layers, ["layer1", "layer0"]);
        assert_eq!(config.timeout, Duration::from_secs(10));
        for suffix in [
            "kumabox.timeout=0",
            "kumabox.timeout=121",
            "kumabox.timeout=bad",
            "kumabox.layers=a,,b",
            "kumabox.layers=a,a",
            "kumabox.cow=../cow",
            "kumabox.hostname=../guest",
        ] {
            assert!(
                Config::parse(&format!("{BASE} {suffix}")).is_err(),
                "{suffix}"
            );
        }
        assert!(Config::parse("kumabox.cow=cow").is_err());
        assert!(!debug_requested("kumabox.debug=1 kumabox.debug=0"));
        assert!(debug_requested("kumabox.debug"));
    }

    #[test]
    fn validates_masks_and_multiple_nics() {
        let cfg = Config::parse(&format!("{BASE} ip=10.88.0.2::10.88.0.1:255.255.0.0:guest:eth0:off:1.1.1.1 ip=169.254.240.2::169.254.240.1:255.255.255.252:guest:eth1:off:8.8.8.8")).unwrap();
        assert_eq!(cfg.networks.len(), 2);
        assert_eq!(cfg.networks[0].prefix, 16);
        let unit = cfg.networks[1].unit("02:00:00:00:00:01");
        assert!(unit.contains("Scope=global"));
        assert!(unit.contains("PreferredSource=169.254.240.2"));
        assert!(unit.contains("DNSDefaultRoute=yes"));
        assert!(!unit.contains("\nGateway=10.88"));
        assert!(Network::parse("10.0.0.2::10.0.0.1:255.0.255.0:g:eth0:off").is_err());
        assert!(Network::parse("10.0.0.2::bad:255.255.255.0:g:eth0:off").is_err());
        assert!(Network::parse("10.0.0.2::10.0.0.1:255.255.255.0:g:../eth0:off").is_err());
    }
}
