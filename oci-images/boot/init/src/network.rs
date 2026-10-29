// SPDX-License-Identifier: MIT
//! Configure static guest links directly through rtnetlink before agent ready.
//! The host owns TAP/eBPF translation; guest identity stays template-local.

use crate::config::Config;
use std::ffi::CString;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::time::Instant;

pub fn configure(cfg: &Config) -> io::Result<()> {
    let mut route = RouteSocket::open()?;
    route.link_up(interface_index("lo")?)?;
    for (order, nic) in cfg.networks.iter().enumerate() {
        let started = Instant::now();
        let index = loop {
            match interface_index(&nic.interface) {
                Ok(index) => break index,
                Err(e) if started.elapsed() >= cfg.timeout => return Err(e),
                Err(_) => std::thread::sleep(std::time::Duration::from_millis(2)),
            }
        };
        route.link_up(index)?;
        let mut address = vec![libc::AF_INET as u8, nic.prefix as u8, 0, 0];
        address.extend(index.to_ne_bytes());
        attribute(&mut address, 1, &nic.address.octets()); // IFA_ADDRESS
        attribute(&mut address, 2, &nic.address.octets()); // IFA_LOCAL
        route.request(20, address)?; // RTM_NEWADDR
        if let Some(gateway) = nic.gateway {
            // Global address scope and explicit preferred source are required
            // for the eBPF provider's routable link-local guest addresses.
            let mut default = vec![libc::AF_INET as u8, 0, 0, 0, 254, 4, 0, 1];
            default.extend(0_u32.to_ne_bytes());
            attribute(&mut default, 4, &index.to_ne_bytes()); // RTA_OIF
            attribute(&mut default, 5, &gateway.octets());
            attribute(&mut default, 7, &nic.address.octets()); // RTA_PREFSRC
            attribute(&mut default, 6, &(100 + order as u32).to_ne_bytes());
            route.request(24, default)?; // RTM_NEWROUTE
        }
    }
    Ok(())
}

fn interface_index(name: &str) -> io::Result<u32> {
    let name = CString::new(name).map_err(|_| io::Error::from(io::ErrorKind::InvalidInput))?;
    // SAFETY: name is a live NUL-terminated interface name.
    let index = unsafe { libc::if_nametoindex(name.as_ptr()) };
    if index == 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(index)
    }
}

fn attribute(message: &mut Vec<u8>, kind: u16, data: &[u8]) {
    let length = 4 + data.len();
    message.extend((length as u16).to_ne_bytes());
    message.extend(kind.to_ne_bytes());
    message.extend(data);
    message.resize((message.len() + 3) & !3, 0);
}

struct RouteSocket {
    fd: OwnedFd,
    sequence: u32,
}

impl RouteSocket {
    fn open() -> io::Result<Self> {
        // SAFETY: socket returns a new owned descriptor or -1.
        let fd = unsafe {
            libc::socket(
                libc::AF_NETLINK,
                libc::SOCK_RAW | libc::SOCK_CLOEXEC,
                libc::NETLINK_ROUTE,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: fd is a newly created, uniquely owned descriptor.
        let fd = unsafe { OwnedFd::from_raw_fd(fd) };
        let timeout = libc::timeval {
            tv_sec: 2,
            tv_usec: 0,
        };
        // SAFETY: timeout is live with exactly the stated size.
        if unsafe {
            libc::setsockopt(
                fd.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_RCVTIMEO,
                (&timeout as *const libc::timeval).cast(),
                size_of::<libc::timeval>() as _,
            )
        } < 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(Self { fd, sequence: 0 })
    }

    fn link_up(&mut self, index: u32) -> io::Result<()> {
        let mut info = vec![0; 4];
        info.extend(index.to_ne_bytes());
        info.extend((libc::IFF_UP as u32).to_ne_bytes());
        info.extend((libc::IFF_UP as u32).to_ne_bytes());
        self.request(16, info) // RTM_NEWLINK
    }

    fn request(&mut self, kind: u16, payload: Vec<u8>) -> io::Result<()> {
        self.sequence += 1;
        let mut message = Vec::with_capacity(16 + payload.len());
        message.extend(((16 + payload.len()) as u32).to_ne_bytes());
        message.extend(kind.to_ne_bytes());
        let flags = if kind == 16 {
            1_u16 | 4
        } else {
            1_u16 | 4 | 0x100 | 0x400
        };
        message.extend(flags.to_ne_bytes()); // REQUEST ACK [REPLACE CREATE]
        message.extend(self.sequence.to_ne_bytes());
        message.extend(0_u32.to_ne_bytes());
        message.extend(payload);
        // SAFETY: zero is a valid sockaddr_nl initialization.
        let mut destination: libc::sockaddr_nl = unsafe { std::mem::zeroed() };
        destination.nl_family = libc::AF_NETLINK as _;
        // SAFETY: live message and destination buffers have their exact lengths.
        if unsafe {
            libc::sendto(
                self.fd.as_raw_fd(),
                message.as_ptr().cast(),
                message.len(),
                0,
                (&destination as *const libc::sockaddr_nl).cast(),
                size_of::<libc::sockaddr_nl>() as _,
            )
        } < 0
        {
            return Err(io::Error::last_os_error());
        }
        let mut reply = [0_u8; 4096];
        loop {
            // SAFETY: reply is writable for its complete stated length.
            let read = unsafe {
                libc::recv(
                    self.fd.as_raw_fd(),
                    reply.as_mut_ptr().cast(),
                    reply.len(),
                    0,
                )
            };
            if read < 0 {
                let error = io::Error::last_os_error();
                if error.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(error);
            }
            if read < 20 {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "short rtnetlink acknowledgement",
                ));
            }
            if u32::from_ne_bytes(reply[8..12].try_into().unwrap()) != self.sequence {
                continue;
            }
            if u16::from_ne_bytes(reply[4..6].try_into().unwrap()) != 2 {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "unexpected rtnetlink acknowledgement",
                ));
            }
            let error = i32::from_ne_bytes(reply[16..20].try_into().unwrap());
            return if error == 0 {
                Ok(())
            } else {
                Err(io::Error::from_raw_os_error(-error))
            };
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn attributes_include_length_and_four_byte_alignment() {
        let mut data = Vec::new();
        attribute(&mut data, 7, &[1, 2, 3]);
        assert_eq!(&data[..2], &7_u16.to_ne_bytes());
        assert_eq!(&data[2..4], &7_u16.to_ne_bytes());
        assert_eq!(&data[4..], &[1, 2, 3, 0]);
    }
}
