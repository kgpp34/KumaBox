from gen_common import *
W, H = 1200, 780
s = svg_open(W, H, "KumaBox architecture: a short-lived CLI control plane driving Cloud Hypervisor microVMs")
s += t(48, 58, "How KumaBox works", 26, FROST, 700)
s += t(48, 88, "Each command is a short-lived process. Only the microVMs keep running.", 16, MUTED)

# ---- left column: control plane
L, LW = 48, 472
s += box(L, 116, LW, 56, fill=PANEL2)
s += t(L+LW/2, 150, "Your agent, script or CI job", 17, FROST, 600, "middle")
s += line(L+LW/2, 172, L+LW/2, 214)
s += t(L+LW/2+14, 199, "kumabox run | exec | clone --json", 13, MUTED, cls="mono")

s += box(L, 220, LW, 236, fill=PANEL, stroke=SLATE, sw=1.6)
s += t(L+24, 256, "kumabox", 20, FROST, 700, cls="mono")
s += t(L+130, 256, "one process per command", 14, MUTED)
chips = [("Resource locks", "safe concurrent commands"),
         ("Operation journal", "resumes interrupted work"),
         ("Metadata", "JSON or SQLite backend"),
         ("Reconcile and GC", "records match reality")]
cw = (LW - 48 - 14) / 2
for i, (a, b) in enumerate(chips):
    x = L + 24 + (i % 2) * (cw + 14); y = 276 + (i // 2) * 66
    s += box(x, y, cw, 54, fill=PANEL2, rx=10)
    s += t(x+14, y+23, a, 15, FROST, 600)
    s += t(x+14, y+43, b, 12.5, MUTED)
s += f'<circle cx="{L+30}" cy="428" r="5" fill="{HONEY}"/>\n'
s += t(L+44, 433, "Exits when the work is done. Nothing idles on the host.", 14, FROST)

s += t(L, 492, "Subsystems each command drives", 14, MUTED, 600)
cards = [("Images", "OCI to shared EROFS layers", "+ private copy-on-write disk"),
         ("Network", "CNI netns per VM", "multiqueue TAP, tc redirect"),
         ("Snapshots", "running or stopped, verified", "clone, hibernate, restore"),
         ("Devices", "hotplug disks, virtio-fs", "VFIO PCI passthrough")]
cw2 = (LW - 16) / 2
for i, (a, b, c) in enumerate(cards):
    x = L + (i % 2) * (cw2 + 16); y = 506 + (i // 2) * 118
    s += box(x, y, cw2, 104, fill=PANEL, rx=12)
    s += f'<rect x="{x}" y="{y+18}" width="4" height="26" rx="2" fill="{SLATE_L}"/>\n'
    s += t(x+20, y+37, a, 17, FROST, 700)
    s += t(x+20, y+64, b, 13, MUTED)
    s += t(x+20, y+84, c, 13, MUTED)

# ---- right column: host
R, RW = 600, 552
s += box(R, 116, RW, 616, fill="#1A2638", stroke=LINE, rx=16, dash="5 6")
s += t(R+24, 148, "Linux host with KVM", 16, FROST, 700)
s += t(R+RW-24, 148, "amd64 or arm64", 13, MUTED, anchor="end")

vx, vw = R+24, RW-48
s += box(vx, 168, vw, 304, fill=PANEL, stroke=SLATE_L, sw=1.6)
s += t(vx+20, 196, "cloud-hypervisor", 15, FROST, 700, cls="mono")
s += t(vx+180, 196, "VMM process for vm-1", 13, MUTED)
gx, gw = vx+20, vw-40
s += box(gx, 212, gw, 244, fill=PANEL2, rx=10)
s += t(gx+18, 238, "microVM guest", 14, FROST, 700)
s += t(gx+gw-18, 238, "hardware boundary", 12.5, MUTED, anchor="end")
layers = [("kumabox-agent on vsock :1024", HONEY, "#2B2A22"),
          ("your workload: shell, code, tools", SLATE_L, "#22344A"),
          ("rootfs: shared EROFS + private COW disk", LINE, "#1D2B3D"),
          ("dedicated Linux guest kernel", LINE, "#18253A")]
lx, lw = gx+18, gw-36
for i, (lab, st, fl) in enumerate(layers):
    y = 254 + i * 48
    s += box(lx, y, lw, 40, fill=fl, stroke=st, rx=8, sw=1.4)
    s += t(lx+16, y+26, lab, 13.5, HONEY if i == 0 else FROST, 600 if i == 0 else 400, cls="mono" if i == 0 else "")

# more VMs
s += t(vx, 506, "vm-2 ... vm-N, one VMM process each", 14, MUTED)
for i in range(6):
    cxp = vx + 34 + i * 82
    if i == 5:
        s += t(cxp, 560, "...", 22, MUTED, 700, "middle"); continue
    s += cube(cxp, 540, 22, top=SLATE_L)
    s += t(cxp, 598, f"vm-{i+2}", 12, MUTED, anchor="middle", cls="mono")

s += box(vx, 618, vw, 42, fill="#1D2B3D", rx=10)
s += t(vx+18, 644, "Shared EROFS layer store: read-only, deduplicated across VMs", 13.5, FROST)
s += box(vx, 670, vw, 42, fill="#1D2B3D", rx=10)
s += t(vx+18, 696, "CNI network: per-VM netns and TAP, NAT to the outside", 13.5, FROST)

# ---- connections
s += path(f"M{L+LW} 300 C {L+LW+50} 300, {lx-60} 274, {lx-4} 274", color=HONEY, sw=2.2, marker="arrH", cls="flow")
s += t(L+LW+40, 262, "vsock", 12.5, HONEY, 600, "middle", "mono")
s += path(f"M{L+LW} 392 L {vx-4} 392", color=SLATE_L, sw=2)
s += t(L+LW+40, 382, "VMM API", 12.5, SLATE_L, 600, "middle", "mono")
s += t(L+LW+40, 758, "", 1)
s += '</svg>\n'
open(os.path.join(OUT, "architecture.svg"), "w").write(s)
print("arch ok")
