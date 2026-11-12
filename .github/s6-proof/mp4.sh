#!/bin/bash
# usage: mp4.sh <frames-dir> <width> <out.mp4> : caption bar on top, 2 s per frame, H.264
set -euo pipefail
dir=$1; w=$2; out=$3
font=/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf
cap="$dir/captioned"; mkdir -p "$cap"
for p in "$dir"/*-"$w".png; do
  b=$(basename "${p%.png}")
  ffmpeg -loglevel error -y -i "$p" -vf "pad=iw:ih+80:0:80:color=#1a1b26,drawtext=fontfile=$font:textfile=$dir/$b.caption:fontcolor=#c0caf5:fontsize=34:x=24:y=24,scale=trunc(iw/2)*2:trunc(ih/2)*2" "$cap/$b.png"
done
ffmpeg -loglevel error -y -framerate 0.5 -pattern_type glob -i "$cap/*-$w.png" -c:v libx264 -pix_fmt yuv420p -r 30 "$out"
ffprobe -v error -show_entries stream=codec_name,width,height,pix_fmt -show_entries format=duration -of compact "$out"
