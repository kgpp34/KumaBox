from gen_common import *
W, H = 1200, 430
style = """
  .fan { opacity: 0; animation: fan 5s ease-out infinite; }
  .f1 { animation-delay: .2s; } .f2 { animation-delay: .5s; } .f3 { animation-delay: .8s; }
  @keyframes fan { 0% {opacity:0;} 14% {opacity:1;} 88% {opacity:1;} 100% {opacity:0;} }
  @media (prefers-reduced-motion: reduce) { .fan { opacity: 1; } }
"""
s = svg_open(W, H, "Sandbox lifecycle: build once, warm once, fork many", style)
s += t(48, 58, "Warm once, fork many", 26, FROST, 700)
s += t(48, 88, "Pay the setup cost one time, then hand every agent attempt its own copy of a ready machine.", 16, MUTED)

steps = [(120, "OCI image", "any digest-pinned ref", SLATE_L, "image build"),
         (330, "EROFS layers", "shared, read-only", SLATE_L, "run"),
         (540, "Running microVM", "agent ready on vsock", SLATE_L, "exec"),
         (750, "Warmed sandbox", "deps installed, caches hot", SLATE_L, "snapshot"),
         (960, "Running snapshot", "memory + disks captured", HONEY, "clone")]
cy = 190
for i, (x, a, b, top, verb) in enumerate(steps):
    s += cube(x, cy, 34, top=top)
    s += f'<circle cx="{x-52}" cy="{cy-34}" r="12" fill="{PANEL2}" stroke="{LINE}"/>' + t(x-52, cy-29.5, str(i+1), 13, MUTED, 700, "middle")
    s += t(x, cy+92, a, 16, HONEY if top == HONEY else FROST, 700, "middle")
    s += t(x, cy+114, b, 13, MUTED, anchor="middle")
    if i < len(steps) - 1:
        nx = steps[i+1][0]
        s += line(x+40, cy+20, nx-42, cy+20)
        s += t((x+nx)/2, cy+8, verb, 13, SLATE_L, 600, "middle", "mono")

# fan out
fx = 1110

for i, dy in enumerate([-70, 0, 70], 1):
    s += f'<g class="fan f{i}">'
    s += path(f"M1000 {cy+20} C 1040 {cy+20}, 1050 {cy+20+dy}, {fx-30} {cy+20+dy}", color=HONEY, sw=1.6, marker=None, dash="4 6")
    s += cube(fx, cy+6+dy, 18, top=SLATE_L)
    s += '</g>\n'
s += t(fx, cy+132, "clone x N", 16, FROST, 700, "middle")
s += t(fx, cy+154, "new IP, new identity", 13, MUTED, anchor="middle")

# side branches from snapshot
by = 368
opts = ["restore in place to roll back", "export and import to another host", "hibernate: free the VMM, keep the state"]
x = 48
s += t(48, by-16, "The same snapshot also lets you", 14, MUTED, 600)
for o in opts:
    p, w = pill(x, by, o, size=13.5, h=32, pad=16, cw=7.3)
    s += p; x += w + 12
s += '</svg>\n'
open(os.path.join(OUT, "lifecycle.svg"), "w").write(s)
print("life ok")
