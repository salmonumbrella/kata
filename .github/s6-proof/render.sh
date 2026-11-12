#!/bin/bash
# usage: render.sh <frames-dir> <repo-dir>
# ANSI -> normalize-ansi.py -> Freeze 0.2.2 SVG -> rsvg-convert PNG (fontconfig fonts).
# Freeze's built-in PNG rasterizer drops text for the system font list in freeze.json.
set -euo pipefail
dir=$1; repo=$2
for f in "$dir"/*.ansi; do
  python3 "$repo/docs/screenshots/normalize-ansi.py" < "$f" |
    freeze --language ansi -c "$repo/docs/screenshots/freeze.json" -o "${f%.ansi}.svg"
  rsvg-convert --zoom 2 -o "${f%.ansi}.png" "${f%.ansi}.svg"
done
