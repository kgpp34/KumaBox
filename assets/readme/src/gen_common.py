import os
OUT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # assets/readme/
# Shared design tokens + helpers for KumaBox README graphics.
BG      = "#172233"   # hull navy
PANEL   = "#1F2D40"   # logo dark face
PANEL2  = "#243449"
SLATE   = "#4E6E8E"   # logo light face
SLATE_L = "#6F8FAF"
LINE    = "#34485F"
FROST   = "#E8EEF5"
MUTED   = "#98AABF"
HONEY   = "#F5B83D"   # kuma loves honey: the single accent
HONEY_D = "#C98E1F"

SANS = "'Inter','Segoe UI','SF Pro Text',-apple-system,'Helvetica Neue',Arial,sans-serif"
MONO = "'JetBrains Mono','SFMono-Regular',Menlo,Consolas,'DejaVu Sans Mono',monospace"

def esc(s):
    return s.replace("&","&amp;").replace("<","&lt;").replace(">","&gt;")

def svg_open(w, h, title, extra_style=""):
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}" role="img" aria-label="{esc(title)}">
<title>{esc(title)}</title>
<defs>
  <pattern id="iso" width="40" height="23.094" patternUnits="userSpaceOnUse">
    <path d="M0 0 L40 23.094 M40 0 L0 23.094" stroke="#ffffff" stroke-opacity="0.028" stroke-width="1"/>
  </pattern>
  <radialGradient id="glow" cx="50%" cy="50%" r="50%">
    <stop offset="0%" stop-color="{HONEY}" stop-opacity="0.35"/>
    <stop offset="100%" stop-color="{HONEY}" stop-opacity="0"/>
  </radialGradient>
  <radialGradient id="vignette" cx="30%" cy="20%" r="90%">
    <stop offset="0%" stop-color="#22324A"/>
    <stop offset="100%" stop-color="{BG}"/>
  </radialGradient>
  <marker id="arr" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M0 0 L10 5 L0 10 z" fill="{SLATE_L}"/>
  </marker>
  <marker id="arrH" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M0 0 L10 5 L0 10 z" fill="{HONEY}"/>
  </marker>
</defs>
<style>
  text {{ font-family: {SANS}; }}
  .mono {{ font-family: {MONO}; }}
  .flow {{ stroke-dasharray: 6 8; animation: flow 1.6s linear infinite; }}
  @keyframes flow {{ to {{ stroke-dashoffset: -28; }} }}
  {extra_style}
  @media (prefers-reduced-motion: reduce) {{ * {{ animation: none !important; }} }}
</style>
<rect x="0" y="0" width="{w}" height="{h}" rx="20" fill="url(#vignette)"/>
<rect x="0" y="0" width="{w}" height="{h}" rx="20" fill="url(#iso)"/>
<rect x="0.5" y="0.5" width="{w-1}" height="{h-1}" rx="20" fill="none" stroke="{LINE}"/>
'''

def t(x, y, s, size=16, fill=FROST, weight=400, anchor="start", cls="", extra=""):
    c = f' class="{cls}"' if cls else ""
    return f'<text x="{x}" y="{y}" font-size="{size}" fill="{fill}" font-weight="{weight}" text-anchor="{anchor}"{c} {extra}>{esc(s)}</text>\n'

def cube(cx, cy, s, top=SLATE_L, left=PANEL, right=SLATE, stroke="#0F1826", sw=2, extra=""):
    """Isometric cube; (cx,cy) is the centre of the top face, s = half-diagonal."""
    k = 0.866 * s
    h = s * 1.15
    topf  = f"{cx},{cy-s/2} {cx+k},{cy} {cx},{cy+s/2} {cx-k},{cy}"
    leftf = f"{cx-k},{cy} {cx},{cy+s/2} {cx},{cy+s/2+h} {cx-k},{cy+h}"
    rightf= f"{cx+k},{cy} {cx},{cy+s/2} {cx},{cy+s/2+h} {cx+k},{cy+h}"
    return (f'<g {extra}><polygon points="{leftf}" fill="{left}" stroke="{stroke}" stroke-width="{sw}" stroke-linejoin="round"/>'
            f'<polygon points="{rightf}" fill="{right}" stroke="{stroke}" stroke-width="{sw}" stroke-linejoin="round"/>'
            f'<polygon points="{topf}" fill="{top}" stroke="{stroke}" stroke-width="{sw}" stroke-linejoin="round"/></g>\n')

def box(x, y, w, h, fill=PANEL, stroke=LINE, rx=12, sw=1.2, dash=None, extra=""):
    d = f' stroke-dasharray="{dash}"' if dash else ""
    return f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{rx}" fill="{fill}" stroke="{stroke}" stroke-width="{sw}"{d} {extra}/>\n'

def pill(x, y, label, fill=PANEL2, stroke=LINE, color=FROST, size=14, pad=14, h=30, mono=False, cw=None):
    # rough width estimate (DejaVu is wide; be generous)
    cw = cw or (size * (0.62 if mono else 0.58))
    w = int(len(label) * cw + pad * 2)
    cls = "mono" if mono else ""
    return (box(x, y, w, h, fill=fill, stroke=stroke, rx=h/2) +
            t(x + w/2, y + h/2 + size*0.36, label, size=size, fill=color, anchor="middle", cls=cls)), w

def line(x1, y1, x2, y2, color=SLATE_L, sw=2, marker="arr", cls="", dash=None):
    m = f' marker-end="url(#{marker})"' if marker else ""
    c = f' class="{cls}"' if cls else ""
    d = f' stroke-dasharray="{dash}"' if dash else ""
    return f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{color}" stroke-width="{sw}" stroke-linecap="round"{m}{c}{d}/>\n'

def path(d, color=SLATE_L, sw=2, marker="arr", cls="", fill="none", dash=None):
    m = f' marker-end="url(#{marker})"' if marker else ""
    c = f' class="{cls}"' if cls else ""
    ds = f' stroke-dasharray="{dash}"' if dash else ""
    return f'<path d="{d}" stroke="{color}" stroke-width="{sw}" fill="{fill}" stroke-linecap="round" stroke-linejoin="round"{m}{c}{ds}/>\n'

def bear_box(cx, cy, s=1.0):
    """The KumaBox mark: a bear peeking out of an isometric box. (cx,cy)=box top centre."""
    g = [f'<g transform="translate({cx},{cy}) scale({s})">']
    k = 86.6; hh = 50
    # back rim of box (behind bear)
    g.append(f'<polygon points="0,-50 {k},0 0,50 {-k},0" fill="#0F1826"/>')
    # bear head
    g.append('<circle cx="-42" cy="-78" r="17" fill="#fff" stroke="#0F1826" stroke-width="4"/>')
    g.append('<circle cx="42" cy="-78" r="17" fill="#fff" stroke="#0F1826" stroke-width="4"/>')
    g.append('<circle cx="-42" cy="-78" r="7" fill="#0F1826"/><circle cx="42" cy="-78" r="7" fill="#0F1826"/>')
    g.append('<ellipse cx="0" cy="-40" rx="58" ry="54" fill="#fff" stroke="#0F1826" stroke-width="4"/>')
    g.append('<ellipse cx="-20" cy="-48" rx="5" ry="7" fill="#0F1826"/><ellipse cx="20" cy="-48" rx="5" ry="7" fill="#0F1826"/>')
    g.append('<ellipse cx="0" cy="-32" rx="9" ry="6" fill="#0F1826"/>')
    g.append('<path d="M-9 -20 Q-4 -15 0 -20 Q4 -15 9 -20" stroke="#0F1826" stroke-width="3" fill="none" stroke-linecap="round"/>')
    # front faces
    g.append(f'<polygon points="{-k},0 0,50 0,{50+hh*2} {-k},{hh*2}" fill="{PANEL}" stroke="#0F1826" stroke-width="4" stroke-linejoin="round"/>')
    g.append(f'<polygon points="{k},0 0,50 0,{50+hh*2} {k},{hh*2}" fill="{SLATE}" stroke="#0F1826" stroke-width="4" stroke-linejoin="round"/>')
    # paws
    g.append('<ellipse cx="-44" cy="16" rx="19" ry="13" fill="#fff" stroke="#0F1826" stroke-width="4" transform="rotate(22 -44 16)"/>')
    g.append('<ellipse cx="44" cy="16" rx="19" ry="13" fill="#fff" stroke="#0F1826" stroke-width="4" transform="rotate(-22 44 16)"/>')
    # prompt glyph on left face, honey
    g.append(f'<path d="M-66 52 L-54 64 L-66 72" stroke="{HONEY}" stroke-width="5" fill="none" stroke-linecap="round" stroke-linejoin="round"/>')
    g.append(f'<path d="M-46 80 L-30 88" stroke="{HONEY}" stroke-width="5" stroke-linecap="round"/>')
    # small cube glyph on right face
    g.append('<g transform="translate(44,78) scale(0.9)" stroke="#fff" stroke-width="3.5" fill="none" stroke-linejoin="round">'
             '<polygon points="0,-16 17,-7 0,2 -17,-7"/><polyline points="-17,-7 -17,12 0,21 17,12 17,-7"/><line x1="0" y1="2" x2="0" y2="21"/></g>')
    g.append('</g>')
    return "\n".join(g) + "\n"
