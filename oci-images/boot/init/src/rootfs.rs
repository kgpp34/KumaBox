// SPDX-License-Identifier: MIT
//! Filesystem contract shared by native boot and unprivileged regression tests.

use std::collections::HashMap;
use std::fs;
use std::io::{self, Read};
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
        let mut bytes = [0_u8; 16];
        fs::File::open("/dev/urandom")?.read_exact(&mut bytes)?;
        let id: String = bytes.iter().map(|b| format!("{b:02x}")).collect();
        fs::write(root.join("etc/machine-id"), format!("{id}\n"))?;
    }
    fs::write(root.join("etc/hostname"), format!("{hostname}\n"))
}

fn remove_if_present(path: &Path) -> io::Result<()> {
    match fs::remove_file(path) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        result => result,
    }
}

/// Publish plain resolver configuration; no local DNS daemon is required.
#[cfg(target_os = "linux")]
pub fn prepare_resolver(root: &Path, cfg: &Config) -> io::Result<()> {
    let dns: Vec<_> = cfg.networks.iter().flat_map(|nic| &nic.dns).collect();
    if dns.is_empty() {
        return Ok(());
    }
    let mut content = String::new();
    let mut seen = std::collections::HashSet::new();
    for ip in dns {
        if seen.insert(ip) {
            content.push_str(&format!("nameserver {ip}\n"));
        }
    }
    let resolver = root.join("etc/resolv.conf");
    remove_if_present(&resolver)?;
    fs::write(resolver, content)
}

/// Preserve the common virtio serial aliases without a udev daemon.
#[cfg(target_os = "linux")]
pub fn device_aliases(sys: &Path, dev: &Path) -> io::Result<()> {
    use std::os::unix::fs::symlink;
    let directory = dev.join("disk/by-id");
    fs::create_dir_all(&directory)?;
    for entry in fs::read_dir(sys)? {
        let entry = entry?;
        if let Ok(serial) = fs::read_to_string(entry.path().join("serial")) {
            let serial = serial.trim();
            if !serial.is_empty()
                && serial
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'-'))
            {
                let link = directory.join(format!("virtio-{serial}"));
                if fs::symlink_metadata(&link).is_err() {
                    symlink(std::path::Path::new("../..").join(entry.file_name()), link)?;
                }
            }
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
    fn first_boot_generates_identity_and_restart_preserves_private_identity() {
        let dir = Fixture::new();
        let base = &dir.0;
        fs::create_dir_all(base.join("root/etc")).unwrap();
        fs::create_dir_all(base.join("upper/etc")).unwrap();
        fs::write(base.join("root/etc/machine-id"), "baked").unwrap();
        prepare_identity(&base.join("root"), &base.join("upper"), "guest").unwrap();
        let id = fs::read_to_string(base.join("root/etc/machine-id")).unwrap();
        assert_eq!(id.trim().len(), 32);
        assert!(id.trim().bytes().all(|b| b.is_ascii_hexdigit()));
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
    fn fixed_network_publishes_direct_dns_without_networkd() {
        let dir = Fixture::new();
        let base = &dir.0;
        fs::create_dir_all(base.join("root/etc")).unwrap();
        fs::create_dir_all(base.join("sys/eth0")).unwrap();
        fs::write(base.join("root/etc/resolv.conf"), "").unwrap();
        fs::write(base.join("sys/eth0/address"), "02:00:00:00:00:01\n").unwrap();
        let config = Config::parse("kumabox.layers=l0 kumabox.cow=cow kumabox.hostname=guest ip=169.254.240.2::169.254.240.1:255.255.255.252:guest:eth0:off:1.1.1.1:8.8.8.8").unwrap();
        prepare_resolver(&base.join("root"), &config).unwrap();
        assert!(!base.join("root/etc/systemd").exists());
        assert_eq!(
            fs::read_to_string(base.join("root/etc/resolv.conf")).unwrap(),
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
        prepare_resolver(&base.join("root"), &config).unwrap();
        assert_eq!(
            fs::read_to_string(base.join("root/etc/resolv.conf")).unwrap(),
            "nameserver 10.0.0.53\n"
        );
    }
}
