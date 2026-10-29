// SPDX-License-Identifier: MIT
//! Filesystem contract shared by native boot and unprivileged regression tests.

use std::collections::HashMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

#[cfg(target_os = "linux")]
use crate::config::Config;

// Resolve the whole disk set against one monotonic deadline. Directory entry
// order and /dev/vdX attachment order never select the layer order.
pub fn resolve_disks(
    sys: &Path,
    dev: &Path,
    serials: &[&str],
    timeout: Duration,
) -> Result<Vec<PathBuf>, String> {
    let started = Instant::now();
    let mut found = HashMap::new();
    loop {
        for entry in fs::read_dir(sys).map_err(|e| format!("scan block devices: {e}"))? {
            let entry = entry.map_err(|e| format!("read block device: {e}"))?;
            let node = dev.join(entry.file_name());
            if !node.exists() {
                continue;
            }
            let serial = fs::read_to_string(entry.path().join("serial"))
                .or_else(|_| fs::read_to_string(entry.path().join("device/serial")));
            if let Ok(serial) = serial {
                let serial = serial.trim();
                if serials.contains(&serial)
                    && let Some(previous) = found.insert(serial.to_owned(), node.clone())
                    && previous != node
                {
                    return Err(format!("ambiguous disk serial {serial}"));
                }
            }
        }
        if serials.iter().all(|id| found.contains_key(*id)) {
            return serials
                .iter()
                .map(|id| {
                    found
                        .get(*id)
                        .cloned()
                        .ok_or_else(|| format!("unresolved disk {id}"))
                })
                .collect();
        }
        if started.elapsed() >= timeout {
            let missing: Vec<_> = serials
                .iter()
                .filter(|id| !found.contains_key(**id))
                .copied()
                .collect();
            return Err(format!("disk discovery timed out: {}", missing.join(",")));
        }
        std::thread::sleep(Duration::from_millis(2));
    }
}

pub fn prepare_identity(root: &Path, upper: &Path, hostname: &str) -> io::Result<()> {
    fs::create_dir_all(root.join("etc"))?;
    // A real nonempty upper-layer file belongs to this sandbox and survives
    // stop/start. Clear a baked image ID only on its first writable boot.
    let owned = fs::symlink_metadata(upper.join("etc/machine-id"))
        .map(|m| m.is_file() && m.len() > 0)
        .or_else(|e| {
            if e.kind() == io::ErrorKind::NotFound {
                Ok(false)
            } else {
                Err(e)
            }
        })?;
    if !owned {
        remove_if_present(&root.join("etc/machine-id"))?;
        fs::write(root.join("etc/machine-id"), "")?;
    }
    fs::write(root.join("etc/hostname"), format!("{hostname}\n"))
}

fn remove_if_present(path: &Path) -> io::Result<()> {
    match fs::remove_file(path) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        result => result,
    }
}

#[cfg(target_os = "linux")]
pub fn persist_network(root: &Path, sys: &Path, cfg: &Config) -> Result<(), String> {
    use std::os::unix::fs::symlink;
    let resolver = root.join("etc/resolv.conf");
    let info = fs::symlink_metadata(&resolver);
    if matches!(&info, Ok(m) if m.is_file() && m.len() == 0)
        || matches!(&info, Err(e) if e.kind() == io::ErrorKind::NotFound)
    {
        remove_if_present(&resolver).map_err(|e| format!("replace empty resolver: {e}"))?;
        symlink("/run/systemd/resolve/stub-resolv.conf", &resolver)
            .map_err(|e| format!("link resolver: {e}"))?;
    } else if let Err(err) = info {
        return Err(format!("inspect resolver: {err}"));
    }

    let mut macs = HashMap::new();
    let started = Instant::now();
    while macs.len() != cfg.networks.len() {
        for nic in &cfg.networks {
            if macs.contains_key(&nic.interface) {
                continue;
            }
            if let Ok(mac) = fs::read_to_string(sys.join(&nic.interface).join("address")) {
                let mac = mac.trim();
                if mac.len() == 17
                    && mac != "00:00:00:00:00:00"
                    && mac.bytes().enumerate().all(|(i, b)| {
                        if i % 3 == 2 {
                            b == b':'
                        } else {
                            b.is_ascii_hexdigit()
                        }
                    })
                {
                    macs.insert(nic.interface.clone(), mac.to_owned());
                }
            }
        }
        if macs.len() == cfg.networks.len() {
            break;
        }
        if started.elapsed() >= Duration::from_millis(200) {
            return Err("static network interface is unavailable".into());
        }
        std::thread::sleep(Duration::from_millis(2));
    }
    if !cfg.networks.is_empty() {
        fs::create_dir_all(root.join("etc/systemd/network"))
            .map_err(|e| format!("create network configuration directory: {e}"))?;
    }
    for nic in &cfg.networks {
        let mac = macs.get(&nic.interface).ok_or("missing network MAC")?;
        let file = root
            .join("etc/systemd/network")
            .join(format!("10-kumabox-{}.network", mac.replace(':', "")));
        fs::write(file, nic.unit(mac)).map_err(|e| format!("write network configuration: {e}"))?;
        if nic.address.is_link_local() && nic.gateway.is_some() && !nic.dns.is_empty() {
            fs::create_dir_all(root.join("etc/kumabox"))
                .map_err(|e| format!("create DNS directory: {e}"))?;
            let content: String = nic
                .dns
                .iter()
                .map(|ip| format!("nameserver {ip}\n"))
                .collect();
            fs::write(root.join("etc/kumabox/resolv.conf"), content)
                .map_err(|e| format!("write DNS configuration: {e}"))?;
            remove_if_present(&resolver).map_err(|e| format!("replace resolver: {e}"))?;
            symlink("/etc/kumabox/resolv.conf", &resolver)
                .map_err(|e| format!("link fixed resolver: {e}"))?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};
    static NEXT: AtomicU64 = AtomicU64::new(0);
    struct Fixture(PathBuf);
    impl Fixture {
        fn new() -> Self {
            let path = std::env::temp_dir().join(format!(
                "kb-init-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&path).unwrap();
            Self(path)
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn discovers_serials_in_requested_order_and_rejects_missing_nodes() {
        let dir = Fixture::new();
        let base = &dir.0;
        fs::create_dir_all(base.join("sys/vda/device")).unwrap();
        fs::create_dir_all(base.join("sys/vdb")).unwrap();
        fs::create_dir(base.join("dev")).unwrap();
        fs::write(base.join("sys/vda/device/serial"), "lower\n").unwrap();
        fs::write(base.join("sys/vdb/serial"), "upper\n").unwrap();
        fs::write(base.join("dev/vda"), "").unwrap();
        assert!(
            resolve_disks(
                &base.join("sys"),
                &base.join("dev"),
                &["upper", "lower"],
                Duration::ZERO
            )
            .is_err()
        );
        fs::write(base.join("dev/vdb"), "").unwrap();
        assert_eq!(
            resolve_disks(
                &base.join("sys"),
                &base.join("dev"),
                &["upper", "lower"],
                Duration::ZERO
            )
            .unwrap(),
            vec![base.join("dev/vdb"), base.join("dev/vda")]
        );
        assert!(
            resolve_disks(
                &base.join("sys"),
                &base.join("dev"),
                &["absent"],
                Duration::ZERO
            )
            .unwrap_err()
            .contains("absent")
        );
        fs::create_dir_all(base.join("sys/vdc")).unwrap();
        fs::write(base.join("sys/vdc/serial"), "upper").unwrap();
        fs::write(base.join("dev/vdc"), "").unwrap();
        assert!(
            resolve_disks(
                &base.join("sys"),
                &base.join("dev"),
                &["upper"],
                Duration::ZERO
            )
            .unwrap_err()
            .contains("ambiguous")
        );
    }

    #[test]
    fn first_boot_clears_baked_identity_and_restart_preserves_private_identity() {
        let dir = Fixture::new();
        let base = &dir.0;
        fs::create_dir_all(base.join("root/etc")).unwrap();
        fs::create_dir_all(base.join("upper/etc")).unwrap();
        fs::write(base.join("root/etc/machine-id"), "baked").unwrap();
        prepare_identity(&base.join("root"), &base.join("upper"), "guest").unwrap();
        assert_eq!(
            fs::read_to_string(base.join("root/etc/machine-id")).unwrap(),
            ""
        );
        fs::write(base.join("upper/etc/machine-id"), "private").unwrap();
        fs::write(base.join("root/etc/machine-id"), "private").unwrap();
        prepare_identity(&base.join("root"), &base.join("upper"), "guest").unwrap();
        assert_eq!(
            fs::read_to_string(base.join("root/etc/machine-id")).unwrap(),
            "private"
        );
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn fixed_network_keeps_global_scope_and_publishes_direct_dns() {
        let dir = Fixture::new();
        let base = &dir.0;
        fs::create_dir_all(base.join("root/etc")).unwrap();
        fs::create_dir_all(base.join("sys/eth0")).unwrap();
        fs::write(base.join("root/etc/resolv.conf"), "").unwrap();
        fs::write(base.join("sys/eth0/address"), "02:00:00:00:00:01\n").unwrap();
        let config = Config::parse("kumabox.layers=l0 kumabox.cow=cow kumabox.hostname=guest ip=169.254.240.2::169.254.240.1:255.255.255.252:guest:eth0:off:1.1.1.1:8.8.8.8").unwrap();
        persist_network(&base.join("root"), &base.join("sys"), &config).unwrap();
        let unit = fs::read_to_string(
            base.join("root/etc/systemd/network/10-kumabox-020000000001.network"),
        )
        .unwrap();
        assert!(unit.contains("Scope=global"));
        assert!(unit.contains("PreferredSource=169.254.240.2"));
        assert_eq!(
            fs::read_link(base.join("root/etc/resolv.conf")).unwrap(),
            PathBuf::from("/etc/kumabox/resolv.conf")
        );
        assert_eq!(
            fs::read_to_string(base.join("root/etc/kumabox/resolv.conf")).unwrap(),
            "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
        );
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn zero_nic_preserves_operator_dns() {
        let dir = Fixture::new();
        let base = &dir.0;
        fs::create_dir_all(base.join("root/etc")).unwrap();
        fs::write(base.join("root/etc/resolv.conf"), "nameserver 10.0.0.53\n").unwrap();
        let config =
            Config::parse("kumabox.layers=l0 kumabox.cow=cow kumabox.hostname=guest").unwrap();
        persist_network(&base.join("root"), &base.join("absent-sys"), &config).unwrap();
        assert_eq!(
            fs::read_to_string(base.join("root/etc/resolv.conf")).unwrap(),
            "nameserver 10.0.0.53\n"
        );
    }
}
