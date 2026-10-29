// SPDX-License-Identifier: MIT
//! Linux calls used by PID 1. Mounts and handoff never invoke a shell.

use std::ffi::CString;
use std::fs::OpenOptions;
use std::io;
use std::os::fd::IntoRawFd;
use std::ptr;

fn cstring(value: &str) -> io::Result<CString> {
    CString::new(value).map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "embedded NUL"))
}

pub fn mount(
    source: &str,
    destination: &str,
    filesystem: Option<&str>,
    flags: libc::c_ulong,
    options: Option<&str>,
) -> io::Result<()> {
    let source = cstring(source)?;
    let destination = cstring(destination)?;
    let filesystem = filesystem.map(cstring).transpose()?;
    let options = options.map(cstring).transpose()?;
    // SAFETY: owned C strings outlive the call; absent optional arguments are null.
    let status = unsafe {
        libc::mount(
            source.as_ptr(),
            destination.as_ptr(),
            filesystem.as_ref().map_or(ptr::null(), |s| s.as_ptr()),
            flags,
            options.as_ref().map_or(ptr::null(), |s| s.as_ptr()).cast(),
        )
    };
    if status == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

pub fn console() -> io::Result<()> {
    let console = OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/console")?;
    let descriptor = console.into_raw_fd();
    for stream in 0..=2 {
        // SAFETY: the source descriptor stays open until all dup2 calls finish.
        if unsafe { libc::dup2(descriptor, stream) } < 0 {
            let error = io::Error::last_os_error();
            close_extra(descriptor);
            return Err(error);
        }
    }
    close_extra(descriptor);
    Ok(())
}

fn close_extra(descriptor: i32) {
    if descriptor > 2 {
        // SAFETY: this is the owned extra descriptor, never a standard stream.
        unsafe {
            libc::close(descriptor);
        }
    }
}

pub fn hostname(value: &str) -> io::Result<()> {
    // SAFETY: sethostname copies the length-bounded live string.
    if unsafe { libc::sethostname(value.as_ptr().cast(), value.len()) } == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

pub fn switch_root() -> io::Result<()> {
    std::env::set_current_dir("/newroot")?;
    mount(".", "/", None, libc::MS_MOVE, None)?;
    // SAFETY: the NUL-terminated literal is static; cwd is the mounted guest root.
    if unsafe { libc::chroot(c".".as_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    std::env::set_current_dir("/")?;
    Ok(())
}

/// Prepare PTYs, shared memory and inner workload cgroups before agent ready.
/// Host VM resource limits remain the host runtime's responsibility.
pub fn runtime_mounts() -> io::Result<()> {
    use std::fs;
    use std::os::unix::fs::{PermissionsExt, symlink};
    for (path, kind, flags, options) in [
        (
            "/dev/pts",
            "devpts",
            libc::MS_NOSUID | libc::MS_NOEXEC,
            "newinstance,ptmxmode=0666,mode=0620,gid=5",
        ),
        (
            "/dev/shm",
            "tmpfs",
            libc::MS_NOSUID | libc::MS_NODEV,
            "mode=1777,size=50%",
        ),
        (
            "/tmp",
            "tmpfs",
            libc::MS_NOSUID | libc::MS_NODEV,
            "mode=1777,size=25%",
        ),
        (
            "/sys/fs/cgroup",
            "cgroup2",
            libc::MS_NOSUID | libc::MS_NODEV | libc::MS_NOEXEC,
            "nsdelegate",
        ),
    ] {
        fs::create_dir_all(path)?;
        mount(kind, path, Some(kind), flags, Some(options))?;
    }
    match fs::remove_file("/dev/ptmx") {
        Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
        _ => {}
    }
    symlink("pts/ptmx", "/dev/ptmx")?;
    for (target, link) in [
        ("/proc/self/fd", "/dev/fd"),
        ("/proc/self/fd/0", "/dev/stdin"),
        ("/proc/self/fd/1", "/dev/stdout"),
        ("/proc/self/fd/2", "/dev/stderr"),
    ] {
        if !std::path::Path::new(link).exists() {
            symlink(target, link)?;
        }
    }
    fs::create_dir_all("/var/tmp")?;
    fs::set_permissions("/var/tmp", fs::Permissions::from_mode(0o1777))?;
    Ok(())
}

pub fn fatal(error: &str, debug: bool) -> ! {
    eprintln!("kumabox-init: {error}");
    if debug {
        let argv = [c"/bin/sh".as_ptr(), ptr::null()];
        // SAFETY: static executable name and live null-terminated argument array.
        unsafe { libc::execv(argv[0], argv.as_ptr()) };
        eprintln!("kumabox-init: debug shell unavailable");
    }
    // SAFETY: sync and reboot receive no borrowed memory. Power off instead of
    // returning from PID 1 and hiding the cause behind a kernel panic.
    unsafe {
        libc::sync();
        libc::reboot(libc::RB_POWER_OFF);
    }
    loop {
        std::thread::park();
    }
}
