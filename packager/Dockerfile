# LabTV packager: pulls a source stream and re-packages it as HLS
FROM alpine:3.24
RUN apk add --no-cache ffmpeg findutils
COPY packager.sh /usr/local/bin/packager.sh
RUN chmod 755 /usr/local/bin/packager.sh
USER 10001
ENTRYPOINT ["/usr/local/bin/packager.sh"]
