# LabTV transcoder: turns an uploaded MP4 into adaptive HLS (360p + 720p) + poster
FROM alpine:3.24
RUN apk add --no-cache ffmpeg jq findutils
COPY transcode.sh /usr/local/bin/transcode.sh
RUN chmod 755 /usr/local/bin/transcode.sh
USER 10001
ENTRYPOINT ["/usr/local/bin/transcode.sh"]
