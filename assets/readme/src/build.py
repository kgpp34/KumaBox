"""Regenerate every README graphic:  python3 assets/readme/src/build.py"""
import os, runpy, sys
here = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, here)
for name in ["gen_hero", "gen_product", "gen_arch", "gen_life", "gen_compare"]:
    runpy.run_path(os.path.join(here, name + ".py"))
