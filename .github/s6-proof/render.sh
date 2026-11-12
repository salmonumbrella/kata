#!/bin/bash
# usage: render.sh <frames-dir> <repo-dir> : ANSI -> PNG via normalize-ansi.py + Freeze 0.2.2
set -euo pipefail
dir=$1; repo=$2
for f in "$dir"/*.ansi; do
  python3 "$repo/docs/screenshots/normalize-ansi.py" < "$f" |
    freeze --language ansi -c "$repo/docs/screenshots/freeze.json" -o "${f%.ansi}.png"
done
