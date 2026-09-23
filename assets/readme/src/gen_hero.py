from gen_common import *
W, H = 1200, 460
style = """
  .clone { opacity: 0; animation: pop 6s ease-out infinite; }
  .c1 { animation-delay: 0.3s; } .c2 { animation-delay: 0.7s; } .c3 { animation-delay: 1.1s; } .c4 { animation-delay: 1.5s; }
  @keyframes pop { 0% {opacity:0; transform: translateX(-18px);} 12% {opacity:1; transform: translateX(0);} 86% {opacity:1;} 100% {opacity:0;} }
  .pulse { animation: pulse 3s ease-in-out infinite; transform-origin: 820px 205px; }
  @keyframes pulse { 0%,100% { opacity: .55; } 50% { opacity: 1; } }
  @media (prefers-reduced-motion: reduce) { .clone { opacity: 1; } }
"""
s = svg_open(W, H, "KumaBox: a disposable computer for every agent task", style)
s += bear_box(165, 188, 1.05)
# text column
x0 = 330
s += f'<text x="{x0}" y="118" font-size="72" font-weight="800" letter-spacing="-1.5"><tspan fill="{FROST}">Kuma</tspan><tspan fill="{SLATE_L}">Box</tspan></text>\n'
s += t(x0, 178, "A disposable computer", 30, FROST, 600)
s += t(x0, 216, "for every agent task.", 30, FROST, 600)
s += t(x0, 262, "Hardware-isolated microVMs on KVM,", 18, MUTED)
s += t(x0, 288, "driven by one daemonless CLI.", 18, MUTED)

# fleet animation
sx, sy = 820, 190
s += '<circle cx="820" cy="215" r="95" fill="url(#glow)" class="pulse"/>\n'
s += cube(sx, sy, 44, top=HONEY, left=PANEL, right=SLATE)
s += t(sx, 300, "snapshot: base", 14, HONEY, 600, "middle", "mono")
targets = [(1060, 90), (1060, 168), (1060, 246), (1060, 324)]
for i, (tx, ty) in enumerate(targets, 1):
    d = f"M{sx+44} {sy+20} C {sx+140} {sy+20}, {tx-140} {ty+14}, {tx-34} {ty+14}"
    s += path(d, color=SLATE_L, sw=1.6, marker=None, cls="flow")
    s += f'<g class="clone c{i}">' + cube(tx, ty, 24, top=SLATE_L) + t(tx+34, ty+18, f"fresh-{i}", 14, FROST, 500, cls="mono") + '</g>\n'
s += t(940, 410, "$ kumabox clone base --name fresh-N", 14, MUTED, 400, "middle", "mono")

# chips
cx = 60; cy = 384
for label in ["Own kernel per sandbox", "Zero resident daemons", "OCI images", "Running snapshots", "amd64 + arm64", "MIT"]:
    p, w = pill(cx, cy, label, size=13, h=28, pad=12, cw=7.2)
    if cx + w > 790:
        break
    s += p; cx += w + 10
s += '</svg>\n'
open(os.path.join(OUT, "hero.svg"), "w").write(s)
print("hero ok")
