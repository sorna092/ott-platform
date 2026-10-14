# LabTV Studio (admin panel) - static Go binary on a minimal non-root image
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY static ./static
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ott-admin .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ott-admin /ott-admin
USER 65532
EXPOSE 8080
ENTRYPOINT ["/ott-admin"]
