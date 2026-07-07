# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY static ./static
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w" -o /dropbridge .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 app
COPY --from=build /dropbridge /usr/local/bin/dropbridge
ENV DROPBRIDGE_INCOMING=/data/incoming \
    DROPBRIDGE_LISTEN=:8787
EXPOSE 8787
VOLUME ["/data/incoming"]
USER app
ENTRYPOINT ["/usr/local/bin/dropbridge"]
