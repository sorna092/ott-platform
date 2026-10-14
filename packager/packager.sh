#!/bin/sh
# Re-packages SOURCE_URL into a rolling HLS playlist in /hls.
# -c copy = remux only (no re-encoding), so it needs very little CPU.
# A VOD source ends after its duration; the loop restarts it, so it
# behaves like a 24/7 channel.
set -u
: "${SOURCE_URL:?SOURCE_URL is required}"
OUT=/hls
mkdir -p "$OUT"

while true; do
  rm -f "$OUT"/*.ts "$OUT"/*.m3u8
  echo "$(date '+%F %T') starting: $SOURCE_URL"
  ffmpeg -hide_banner -loglevel warning -re \
    -i "$SOURCE_URL" \
    -c copy \
    -f hls -hls_time 4 -hls_list_size 6 \
    -hls_flags delete_segments+independent_segments+omit_endlist \
    -hls_segment_filename "$OUT/seg_%05d.ts" \
    "$OUT/index.m3u8"
  rc=$?
  echo "$(date '+%F %T') ffmpeg exited with code $rc, restarting in 3s"
  sleep 3
done
