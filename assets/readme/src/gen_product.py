from gen_common import *
W, H = 1200, 520
s = svg_open(W, H, "Product shape: callers, interfaces, and what one KumaBox sandbox gives you")
s += t(48, 58, "What KumaBox is", 26, FROST, 700)
s += t(48, 88, "A sandbox runtime you install on one Linux box. Anything that can run a command can drive it.", 16, MUTED)

# left: callers
s += t(48, 140, "Who drives it", 14, MUTED, 600)
callers = ["Coding agents", "Agent frameworks", "RL and eval harnesses", "CI and batch jobs", "You, at a terminal"]
for i, c in enumerate(callers):
    y = 156 + i * 58
    s += box(48, y, 268, 44, fill=PANEL2, rx=22)
    s += t(182, y+28, c, 15, FROST, 500, "middle")

# middle: interfaces
MX, MW = 404, 336
s += t(MX, 140, "How it is driven", 14, MUTED, 600)
s += box(MX, 156, MW, 132, fill=PANEL, stroke=HONEY, sw=1.8)
s += t(MX+22, 192, "kumabox CLI", 20, FROST, 700, cls="mono")
s += t(MX+MW-22, 190, "available now", 12.5, HONEY, 600, "end")
s += t(MX+22, 222, "Human output, or --json for machines", 13.5, MUTED)
s += t(MX+22, 244, "Streams stdout, stderr and exit codes", 13.5, MUTED)
s += t(MX+22, 266, "Dry-run launch plans via kumabox debug", 13.5, MUTED)
planned = ["Go SDK", "HTTP API, E2B-compatible", "MCP server for tool use"]
s += t(MX, 318, "Planned", 13, MUTED, 600)
for i, p in enumerate(planned):
    y = 330 + i * 52
    s += box(MX, y, MW, 42, fill="none", stroke=SLATE, rx=10, dash="5 5")
    s += t(MX+22, y+27, p, 14.5, MUTED, 500)

# right: sandbox
RX, RW = 820, 332
s += t(RX, 140, "What each sandbox gets", 14, MUTED, 600)
s += box(RX, 156, RW, 322, fill=PANEL, stroke=SLATE_L, sw=1.6)
s += cube(RX+46, 190, 22, top=SLATE_L)
s += t(RX+84, 204, "one microVM", 18, FROST, 700)
feats = ["Its own guest kernel behind KVM", "OCI rootfs + private writable disk", "Own network namespace and IP",
         "exec with env, workdir, stdin, TTY", "Snapshot, clone, hibernate, restore", "Opt-in data disks, virtio-fs, VFIO"]
for i, f in enumerate(feats):
    y = 262 + i * 36
    s += f'<path d="M{RX+26} {y-5} l5 5 l9 -10" stroke="{HONEY}" stroke-width="2.4" fill="none" stroke-linecap="round" stroke-linejoin="round"/>\n'
    s += t(RX+50, y, f, 14, FROST)

s += line(324, 222, MX-10, 222)
s += line(MX+MW+8, 222, RX-10, 222)
s += '</svg>\n'
open(os.path.join(OUT, "product.svg"), "w").write(s)
print("product ok")
