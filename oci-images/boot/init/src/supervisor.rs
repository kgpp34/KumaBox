// SPDX-License-Identifier: MIT
//! PID 1 owns agent supervision and adopted orphans, never the agent's children.
//!
//! agent alive -> reap adopted orphans
//! agent exit  -> terminate remaining guest tasks -> bounded restart
//! power event -> TERM -> bounded grace -> KILL -> sync -> power off

use std::fs::{self, File, OpenOptions};
use std::io::{self, Read};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::process::CommandExt;
use std::process::Command;
use std::time::{Duration, Instant};

const PATH: &str = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
const GRACE: Duration = Duration::from_secs(3);

pub fn run() -> io::Result<()> {
    let signals = signal_fd()?;
    let mut agent = spawn_agent()?;
    let mut shell = spawn_console().ok();
    let mut inputs = power_inputs();
    let mut budget = RestartBudget::new();
    let mut next_scan = Instant::now() + Duration::from_secs(1);
    loop {
        let mut polls = vec![libc::pollfd {
            fd: signals.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        }];
        polls.extend(inputs.iter().map(|file| libc::pollfd {
            fd: file.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        }));
        // Signals wake immediately. A slow scan also discovers late ACPI input
        // devices without starting udev or placing discovery on agent readiness.
        // SAFETY: polls is a live array with the exact stated element count.
        let status = unsafe { libc::poll(polls.as_mut_ptr(), polls.len() as _, 1000) };
        if status < 0 && io::Error::last_os_error().kind() != io::ErrorKind::Interrupted {
            return Err(io::Error::last_os_error());
        }
        let mut power = false;
        if polls[0].revents & libc::POLLIN != 0 {
            let mut info = [0_u8; 128];
            // SAFETY: signalfd_siginfo is exactly 128 bytes; info is writable.
            let read =
                unsafe { libc::read(signals.as_raw_fd(), info.as_mut_ptr().cast(), info.len()) };
            if read == info.len() as isize {
                let signal = u32::from_ne_bytes(info[..4].try_into().unwrap()) as i32;
                power = matches!(signal, libc::SIGTERM | libc::SIGINT | libc::SIGPWR);
            }
        }
        for (index, input) in inputs.iter_mut().enumerate() {
            if polls[index + 1].revents & libc::POLLIN != 0 {
                power |= power_pressed(input)?;
            }
        }
        if power {
            eprintln!("kumabox-init: shutdown requested");
            stop_console(shell);
            terminate_tasks();
            return power_off();
        }
        for (pid, status) in reap()?.exited {
            if Some(pid) == shell {
                shell = None;
            }
            if pid == agent {
                eprintln!("kumabox-init: agent exited (wait status {status})");
                // Old detached jobs cannot be safely adopted by a new agent's
                // in-memory process table. End them before accepting new work.
                stop_console(shell);
                terminate_tasks();
                if !budget.allow(Instant::now()) {
                    return Err(io::Error::other("agent restart budget exhausted"));
                }
                agent = spawn_agent()?;
                shell = None;
            }
        }
        if Instant::now() >= next_scan {
            if inputs.is_empty() {
                inputs = power_inputs();
            }
            if shell.is_none() {
                shell = spawn_console().ok();
            }
            next_scan = Instant::now() + Duration::from_secs(1);
        }
    }
}

fn stop_console(shell: Option<i32>) {
    if let Some(pid) = shell {
        // SAFETY: this is the supervisor-owned console child. Interactive
        // shells ignore SIGTERM, so do not spend workload shutdown grace here.
        unsafe {
            libc::kill(pid, libc::SIGKILL);
        }
    }
}

fn spawn_agent() -> io::Result<i32> {
    let mut command = Command::new("/usr/local/bin/kumabox-agent");
    command
        .arg("serve")
        .env_clear()
        .env("PATH", PATH)
        .env("HOME", "/root")
        .env("LANG", "C.UTF-8")
        .env("TERM", "linux");
    // SAFETY: the child only resets its signal mask before exec; no allocation
    // or parent-owned state is touched in this single-threaded supervisor.
    unsafe {
        command.pre_exec(|| reset_child_signals(false));
    }
    Ok(command.spawn()?.id() as i32)
}

fn spawn_console() -> io::Result<i32> {
    let terminal = OpenOptions::new()
        .read(true)
        .write(true)
        .open("/dev/hvc0")?;
    let mut command = Command::new("/bin/sh");
    command
        .arg("-l")
        .env_clear()
        .env("PATH", PATH)
        .env("HOME", "/root")
        .env("TERM", "linux")
        .stdin(terminal.try_clone()?)
        .stdout(terminal.try_clone()?)
        .stderr(terminal);
    // SAFETY: only async-signal-safe session/terminal and signal calls run
    // between fork and exec. Console access already requires host VM access.
    unsafe {
        command.pre_exec(|| reset_child_signals(true));
    }
    Ok(command.spawn()?.id() as i32)
}

fn reset_child_signals(console: bool) -> io::Result<()> {
    // SAFETY: sigset_t accepts zero initialization followed by sigemptyset;
    // setsid and ioctl operate on this child's descriptors only.
    unsafe {
        let mut empty = std::mem::zeroed();
        libc::sigemptyset(&mut empty);
        if libc::sigprocmask(libc::SIG_SETMASK, &empty, std::ptr::null_mut()) != 0 {
            return Err(io::Error::last_os_error());
        }
        if libc::setsid() < 0 {
            return Err(io::Error::last_os_error());
        }
        if console && libc::ioctl(0, libc::TIOCSCTTY, 0) < 0 {
            return Err(io::Error::last_os_error());
        }
    }
    Ok(())
}

fn signal_fd() -> io::Result<OwnedFd> {
    // SAFETY: the initialized set is live through sigprocmask/signalfd. Signals
    // stay blocked in PID 1 and are unblocked explicitly in each child.
    unsafe {
        let mut mask = std::mem::zeroed();
        libc::sigemptyset(&mut mask);
        for signal in [libc::SIGCHLD, libc::SIGTERM, libc::SIGINT, libc::SIGPWR] {
            libc::sigaddset(&mut mask, signal);
        }
        if libc::sigprocmask(libc::SIG_BLOCK, &mask, std::ptr::null_mut()) != 0 {
            return Err(io::Error::last_os_error());
        }
        let fd = libc::signalfd(-1, &mask, libc::SFD_CLOEXEC | libc::SFD_NONBLOCK);
        if fd < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(OwnedFd::from_raw_fd(fd))
        }
    }
}

struct Reaped {
    /// Exit statuses consumed by PID 1; the live agent owns its own children.
    exited: Vec<(i32, i32)>,
    /// At least one direct child is still alive after draining ready statuses.
    children: bool,
}

fn reap() -> io::Result<Reaped> {
    let mut exited = Vec::new();
    loop {
        let mut status = 0;
        // SAFETY: status points to a live integer. Only this process's direct
        // children are waited for, so the live Go agent keeps its own waiters.
        let pid = unsafe { libc::waitpid(-1, &mut status, libc::WNOHANG) };
        if pid > 0 {
            exited.push((pid, status));
            continue;
        }
        if pid == 0 {
            return Ok(Reaped {
                exited,
                children: true,
            });
        }
        let error = io::Error::last_os_error();
        match error.raw_os_error() {
            Some(libc::ECHILD) => {
                return Ok(Reaped {
                    exited,
                    children: false,
                });
            }
            Some(libc::EINTR) => continue,
            _ => return Err(error),
        }
    }
}

fn terminate_tasks() {
    // SAFETY: this function runs only as VM PID 1. kill(-1) excludes itself
    // and signals guest userspace, never any host process.
    unsafe {
        libc::kill(-1, libc::SIGTERM);
    }
    let started = Instant::now();
    while started.elapsed() < GRACE {
        if let Ok(Reaped {
            children: false, ..
        }) = reap()
        {
            return;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    // SAFETY: same PID 1 scope as above; bound stubborn task termination.
    unsafe {
        libc::kill(-1, libc::SIGKILL);
    }
    let started = Instant::now();
    while started.elapsed() < Duration::from_secs(1) {
        if let Ok(Reaped {
            children: false, ..
        }) = reap()
        {
            break;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

fn power_off() -> io::Result<()> {
    // SAFETY: all tasks have been signaled; sync flushes every mounted guest
    // filesystem, including attached data disks, before kernel poweroff.
    unsafe {
        libc::sync();
    }
    // SAFETY: reboot takes no borrowed pointers and is called only by PID 1.
    if unsafe { libc::reboot(libc::RB_POWER_OFF) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Err(io::Error::other("poweroff returned unexpectedly"))
}

fn power_inputs() -> Vec<File> {
    let Ok(entries) = fs::read_dir("/dev/input") else {
        return Vec::new();
    };
    entries
        .filter_map(Result::ok)
        .filter(|e| e.file_name().as_encoded_bytes().starts_with(b"event"))
        .filter_map(|e| {
            OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NONBLOCK | libc::O_CLOEXEC)
                .open(e.path())
                .ok()
        })
        .collect()
}

fn power_pressed(file: &mut File) -> io::Result<bool> {
    let mut bytes = [0_u8; size_of::<libc::input_event>()];
    let mut pressed = false;
    loop {
        match file.read(&mut bytes) {
            Ok(0) => return Ok(pressed),
            Ok(n) if n == bytes.len() => {
                let offset = size_of::<libc::timeval>();
                pressed |= u16::from_ne_bytes(bytes[offset..offset + 2].try_into().unwrap()) == 1
                    && u16::from_ne_bytes(bytes[offset + 2..offset + 4].try_into().unwrap()) == 116
                    && i32::from_ne_bytes(bytes[offset + 4..offset + 8].try_into().unwrap()) == 1;
            }
            Ok(_) => return Err(io::Error::other("short input event")),
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => return Ok(pressed),
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        }
    }
}

struct RestartBudget {
    since: Instant,
    failures: u32,
}
impl RestartBudget {
    fn new() -> Self {
        Self {
            since: Instant::now(),
            failures: 0,
        }
    }
    fn allow(&mut self, now: Instant) -> bool {
        if now.duration_since(self.since) >= Duration::from_secs(30) {
            self.since = now;
            self.failures = 0;
        }
        self.failures += 1;
        self.failures <= 3
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn restart_storm_is_bounded_and_stable_agent_resets_budget() {
        let mut budget = RestartBudget::new();
        let now = budget.since;
        assert!(budget.allow(now));
        assert!(budget.allow(now));
        assert!(budget.allow(now));
        assert!(!budget.allow(now));
        assert!(budget.allow(now + Duration::from_secs(30)));
    }
}
