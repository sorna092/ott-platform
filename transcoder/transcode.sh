#!/bin/sh
# Input : /data/uploads/$INPUT_FILE        (copied in by scripts/upload-movie.sh)
# Output: /data/vod/$MOVIE_ID/master.m3u8  (360p + 720p HLS), poster.jpg, meta.json
#         /data/vod/catalog.json           (list of all movies, read by the web page)
set -eu
: "${MOVIE_ID:?MOVIE_ID is required}"
: "${MOVIE_TITLE:?MOVIE_TITLE is required}"
: "${INPUT_FILE:?INPUT_FILE is required}"

IN="/data/uploads/${INPUT_FILE}"
OUT="/data/vod/${MOVIE_ID}"
TMP="/data/tmp/${MOVIE_ID}"
[ -f "$IN" ] || { echo "Input not found: $IN"; exit 1; }

rm -rf "$TMP"
mkdir -p "$TMP/v0" "$TMP/v1"

DUR=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$IN" | cut -d. -f1)
case "$DUR" in ''|*[!0-9]*) DUR=0 ;; esac
HAS_AUDIO=$(ffprobe -v error -select_streams a:0 -show_entries stream=index -of csv=p=0 "$IN" | head -1)
echo "$(date '+%T') movie=$MOVIE_ID duration=${DUR}s audio=${HAS_AUDIO:-none}"

if [ -n "$HAS_AUDIO" ]; then
  AMAP="-map 0:a:0 -map 0:a:0 -c:a aac -b:a 128k -ac 2"
  VSM="v:0,a:0 v:1,a:1"
else
  AMAP=""
  VSM="v:0 v:1"
fi

echo "$(date '+%T') transcoding to 360p + 720p ..."
# shellcheck disable=SC2086
ffmpeg -hide_banner -loglevel warning -stats -y -i "$IN" \
  -filter_complex "[0:v]split=2[a][b];[a]scale=-2:360[v360];[b]scale=-2:720[v720]" \
  -map "[v360]" -map "[v720]" $AMAP \
  -c:v libx264 -preset veryfast -profile:v main -pix_fmt yuv420p \
  -b:v:0 800k  -maxrate:v:0 900k  -bufsize:v:0 1600k \
  -b:v:1 2500k -maxrate:v:1 2800k -bufsize:v:1 5000k \
  -force_key_frames "expr:gte(t,n_forced*2)" -sc_threshold 0 \
  -f hls -hls_time 4 -hls_playlist_type vod -hls_flags independent_segments \
  -hls_segment_filename "$TMP/v%v/seg_%04d.ts" \
  -master_pl_name master.m3u8 -var_stream_map "$VSM" \
  "$TMP/v%v/index.m3u8"

# Make sure master.m3u8 sits next to v0/ and v1/
if [ ! -f "$TMP/master.m3u8" ]; then
  echo "writing master.m3u8"
  rm -f "$TMP"/v*/master.m3u8
  cat > "$TMP/master.m3u8" <<EOF
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=1050000
v0/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2950000
v1/index.m3u8
EOF
fi

echo "$(date '+%T') creating poster"
SS=10; [ "$DUR" -lt 20 ] && SS=1
ffmpeg -hide_banner -loglevel error -y -ss "$SS" -i "$IN" -frames:v 1 -vf "scale=480:-2" "$TMP/poster.jpg" || true

jq -n --arg id "$MOVIE_ID" --arg title "$MOVIE_TITLE" --argjson dur "$DUR" \
  '{id: $id, title: $title, duration: $dur,
    url: ("/vod/" + $id + "/master.m3u8"),
    poster: ("/vod/" + $id + "/poster.jpg"),
    added: (now | todate)}' > "$TMP/meta.json"

# Publish in one move (viewers never see a half-written movie)
rm -rf "$OUT"
mv "$TMP" "$OUT"

# Rebuild the catalog from every movie's meta.json (newest first)
find /data/vod -mindepth 2 -maxdepth 2 -name meta.json -exec cat {} + \
  | jq -s 'sort_by(.added) | reverse' > /data/vod/catalog.json.tmp
mv /data/vod/catalog.json.tmp /data/vod/catalog.json

[ "${DELETE_UPLOAD:-true}" = "true" ] && rm -f "$IN"
echo "$(date '+%T') DONE: /vod/$MOVIE_ID/master.m3u8 ($(jq length /data/vod/catalog.json) movies in catalog)"
