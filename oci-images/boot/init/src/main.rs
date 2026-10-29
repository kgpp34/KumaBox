// SPDX-License-Identifier: MIT
//! Initramfs PID 1 for the overlay-v1 boot contract.
//!
//! built-in kernel -> discover serials -> EROFS + private ext4 -> overlay
//!                 -> guest identity/network -> switch root -> supervise agent

mod config;
#[cfg(target_os = "linux")]
mod linux;
#[cfg(target_os = "linux")]
mod network;
mod rootfs;
#[cfg(target_os = "linux")]
mod supervisor;

#[cfg(target_os = "linux")]
static DEBUG: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

#[cfg(target_os = "linux")]
fn main() {
    if std::process::id() != 1 {
        eprintln!("kumabox-init: must run as PID 1");
        std::process::exit(1);
    }
    if let Err(error) = boot() {
        linux::fatal(&error, DEBUG.load(std::sync::atomic::Ordering::Relaxed));
    }
}

#[cfg(target_os = "linux")]
fn boot() -> Result<(), String> {
    use std::fs;
    use std::path::Path;
    use std::time::Instant;
    let started = Instant::now();
    let mut marks = Vec::new();
    // The archive contains these directories and an initial /dev/console.
    linux::mount("devtmpfs", "/dev", Some("devtmpfs"), libc::MS_NOSUID, None)
        .map_err(|e| format!("mount /dev: {e}"))?;
    linux::console().map_err(|e| format!("open console: {e}"))?;
    for (kind, target) in [("proc", "/proc"), ("sysfs", "/sys")] {
        linux::mount(
            kind,
            target,
            Some(kind),
            libc::MS_NOSUID | libc::MS_NODEV | libc::MS_NOEXEC,
            None,
        )
        .map_err(|e| format!("mount {target}: {e}"))?;
    }
    let command =
        fs::read_to_string("/proc/cmdline").map_err(|e| format!("read boot arguments: {e}"))?;
    DEBUG.store(
        config::debug_requested(&command),
        std::sync::atomic::Ordering::Relaxed,
    );
    let cfg = config::Config::parse(&command)?;
    marks.push(("mounts", started.elapsed().as_micros()));
    let serials: Vec<_> = cfg
        .layers
        .iter()
        .map(String::as_str)
        .chain([cfg.cow.as_str()])
        .collect();
    let devices = rootfs::resolve_disks(
        Path::new("/sys/block"),
        Path::new("/dev"),
        &serials,
        cfg.timeout,
    )?;
    marks.push(("devices", started.elapsed().as_micros()));
    let mut lower = Vec::new();
    for (index, device) in devices[..cfg.layers.len()].iter().enumerate() {
        let destination = format!("/layers/{index}");
        fs::create_dir_all(&destination).map_err(|e| format!("create layer directory: {e}"))?;
        linux::mount(
            device.to_str().ok_or("invalid device name")?,
            &destination,
            Some("erofs"),
            libc::MS_RDONLY,
            None,
        )
        .map_err(|e| format!("mount layer {index}: {e}"))?;
        lower.push(destination);
    }
    marks.push(("layers", started.elapsed().as_micros()));
    let cow = devices.last().ok_or("missing writable disk")?;
    linux::mount(
        cow.to_str().ok_or("invalid writable device")?,
        "/cow",
        Some("ext4"),
        libc::MS_NOATIME,
        None,
    )
    .map_err(|e| format!("mount writable disk: {e}"))?;
    for directory in ["/cow/upper", "/cow/work"] {
        fs::create_dir_all(directory).map_err(|e| format!("create overlay directory: {e}"))?;
    }
    let options = format!(
        "lowerdir={},upperdir=/cow/upper,workdir=/cow/work,index=on,redirect_dir=on,metacopy=on,xino=on",
        lower.join(":")
    );
    linux::mount("overlay", "/newroot", Some("overlay"), 0, Some(&options))
        .map_err(|e| format!("mount root overlay: {e}"))?;
    marks.push(("overlay", started.elapsed().as_micros()));
    linux::hostname(&cfg.hostname).map_err(|e| format!("set hostname: {e}"))?;
    rootfs::prepare_identity(
        Path::new("/newroot"),
        Path::new("/cow/upper"),
        &cfg.hostname,
    )
    .map_err(|e| format!("prepare identity: {e}"))?;
    rootfs::prepare_resolver(Path::new("/newroot"), &cfg)
        .map_err(|e| format!("prepare DNS: {e}"))?;
    network::configure(&cfg).map_err(|e| format!("configure guest network: {e}"))?;
    rootfs::device_aliases(Path::new("/sys/block"), Path::new("/dev"))
        .map_err(|e| format!("prepare disk aliases: {e}"))?;
    marks.push(("identity-network", started.elapsed().as_micros()));
    for directory in ["dev", "proc", "sys", "run"] {
        fs::create_dir_all(Path::new("/newroot").join(directory))
            .map_err(|e| format!("create handoff directory: {e}"))?;
    }
    // Runtime state is volatile; the private ext4 upper holds installed tools
    // and application files across stop/start.
    linux::mount(
        "tmpfs",
        "/newroot/run",
        Some("tmpfs"),
        libc::MS_NOSUID | libc::MS_NODEV,
        Some("mode=0755"),
    )
    .map_err(|e| format!("mount /run: {e}"))?;
    fs::create_dir_all("/newroot/run/kumabox").map_err(|e| format!("create boot state: {e}"))?;
    fs::write("/newroot/run/kumabox/boot-root-ready", b"overlay-v1\n")
        .map_err(|e| format!("record boot readiness: {e}"))?;
    fs::write("/newroot/run/kumabox/profile", b"native-v1\n")
        .map_err(|e| format!("record runtime profile: {e}"))?;
    for directory in ["dev", "proc", "sys"] {
        linux::mount(
            &format!("/{directory}"),
            &format!("/newroot/{directory}"),
            None,
            libc::MS_MOVE,
            None,
        )
        .map_err(|e| format!("move /{directory}: {e}"))?;
    }
    if cfg.trace {
        eprintln!("kumabox-init: cumulative_us {marks:?}");
    }
    linux::switch_root().map_err(|e| format!("switch root: {e}"))?;
    linux::runtime_mounts().map_err(|e| format!("mount guest runtime: {e}"))?;
    supervisor::run().map_err(|e| format!("supervise agent: {e}"))
}

#[cfg(not(target_os = "linux"))]
fn main() {
    eprintln!("kumabox-init requires Linux");
    std::process::exit(1);
}
