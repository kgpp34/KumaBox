from gen_common import *
W, H = 1200, 664
s = svg_open(W, H, "What you operate to get hardware-isolated sandboxes: KumaBox vs CubeSandbox vs self-hosted E2B")
s += t(48, 58, "What you run on one host to get one kernel per task", 26, FROST, 700)
s += t(48, 88, "Taller stacks buy multi-tenant APIs, scheduling and dashboards. KumaBox keeps only what a single host needs.", 16, MUTED)

cols = [
  ("KumaBox", "MIT, Go", HONEY,
   ["Linux + KVM, amd64 or arm64", "cloud-hypervisor + CNI plugins", "kumabox, one binary"],
   ["No database, no cluster manager,", "no resident daemon."]),
  ("CubeSandbox, one-click node", "Apache-2.0", SLATE_L,
   ["Linux + KVM, x86_64 or ARM64", "CubeHypervisor + CubeShim", "Cubelet + CubeVS eBPF network", "CubeMaster + lifecycle manager",
    "CubeAPI, CubeProxy, CubeEgress", "MySQL, Redis, MinIO", "Web UI + CubeOps"],
   ["Full sandbox service with E2B API.", "Terraform or Kubernetes for clusters."]),
  ("E2B Embed, one machine", "Apache-2.0, Go", SLATE_L,
   ["Linux + KVM + Docker Compose", "Firecracker VMM", "Orchestrator + template builder", "API server + client proxy",
    "PostgreSQL, Redis, ClickHouse", "Dashboard + log pipeline"],
   ["Same runtime as E2B Cloud.", "Terraform or Kubernetes for more nodes."]),
]
base = 548; bh = 44; gap = 8; cw = 344
for ci, (name, lic, accent, blocks, foot) in enumerate(cols):
    x = 48 + ci * (cw + 36)
    s += t(x, 146, name, 19, HONEY if ci == 0 else FROST, 700)
    s += t(x + cw, 146, lic, 13, MUTED, anchor="end")
    s += f'<line x1="{x}" y1="158" x2="{x+cw}" y2="158" stroke="{LINE}"/>\n'
    for bi, label in enumerate(blocks):
        y = base - (bi + 1) * (bh + gap) + gap
        top = bi == len(blocks) - 1
        if ci == 0:
            fill = "#2B2A22" if top else PANEL2; st = HONEY if top else LINE
        else:
            fill = PANEL if bi % 2 == 0 else PANEL2; st = LINE
        s += box(x, y, cw, bh, fill=fill, stroke=st, rx=9, sw=1.4 if top and ci == 0 else 1.1)
        s += t(x + 16, y + 28, label, 14, HONEY if (top and ci == 0) else FROST, 600 if (top and ci == 0) else 400,
               cls="mono" if (top and ci == 0) else "")
    if ci == 0:
        ty = base - len(blocks) * (bh + gap)
        s += path(f"M{x+cw/2} {ty-10} L {x+cw/2} {ty-120}", color=HONEY, sw=1.4, marker=None, dash="3 6")
        s += t(x + cw/2, ty - 134, "That's the whole stack.", 17, HONEY, 700, "middle")
    for li, fl in enumerate(foot):
        s += t(x, base + 30 + li * 20, fl, 13.5, FROST if ci == 0 else MUTED)
s += t(W - 48, H - 18, "Single-host install footprint, from each project's repository and deploy files, September 2026.", 11.5, MUTED, anchor="end")
s += '</svg>\n'
open(os.path.join(OUT, "comparison.svg"), "w").write(s)
print("cmp ok")
