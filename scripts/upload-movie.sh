#!/usr/bin/env bash
# Upload a movie to LabTV and transcode it.  Run on master1:
#   ./scripts/upload-movie.sh /root/movies/sintel.mp4 sintel "Sintel"
set -euo pipefail

FILE=${1:?usage: upload-movie.sh <file.mp4> <movie-id> "<Title>"}
ID=${2:?movie-id missing (lowercase letters, digits, -)}
TITLE=${3:?title missing}
NS=ott
DIR="$(cd "$(dirname "$0")/.." && pwd)"

[[ "$ID" =~ ^[a-z0-9-]{1,40}$ ]] || { echo "movie-id: only lowercase letters, digits and - (max 40)"; exit 1; }
[[ -f "$FILE" ]] || { echo "File not found: $FILE"; exit 1; }
TITLE_SAFE=$(printf '%s' "$TITLE" | tr -cd 'A-Za-z0-9 ._,:()-')

POD=$(kubectl -n "$NS" get pod -l app=vod-store -o jsonpath='{.items[0].metadata.name}')
[[ -n "$POD" ]] || { echo "vod-store pod not found"; exit 1; }

echo ">> 1/3 Uploading $(du -h "$FILE" | cut -f1) to $POD ..."
kubectl -n "$NS" cp "$FILE" "$POD:/data/uploads/$ID.mp4" -c nginx

echo ">> 2/3 Starting transcode job transcode-$ID"
kubectl -n "$NS" delete job "transcode-$ID" --ignore-not-found
sed -e "s|__ID__|$ID|g" -e "s|__TITLE__|$TITLE_SAFE|g" "$DIR/jobs/transcode-job.yaml" | kubectl apply -f -

echo ">> 3/3 Waiting for the job pod, then following its log"
kubectl -n "$NS" wait --for=condition=Ready pod -l job-name="transcode-$ID" --timeout=300s >/dev/null 2>&1 || true
kubectl -n "$NS" logs -f "job/transcode-$ID" || true

if kubectl -n "$NS" wait --for=condition=complete "job/transcode-$ID" --timeout=10s >/dev/null 2>&1; then
  echo ">> SUCCESS: open http://tv.lab.local and look under Movies"
else
  echo ">> Job did not complete. Check: kubectl -n $NS describe job transcode-$ID"
  exit 1
fi
