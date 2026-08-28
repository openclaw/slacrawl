# syntax=docker/dockerfile:1.26

ARG GO_VERSION=1.27.0
ARG ALPINE_VERSION=3.24

FROM golang:${GO_VERSION}-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/openclaw/slacrawl/internal/cli.version=${VERSION}" -o /out/slacrawl ./cmd/slacrawl

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates git nodejs npm openssh-client tzdata \
    && adduser -D -u 10001 -h /data slacrawl \
    && mkdir -p /data \
    && chown -R slacrawl:slacrawl /data
ENV HOME=/data
VOLUME ["/data"]
WORKDIR /data
COPY --from=build /out/slacrawl /usr/local/bin/slacrawl
USER slacrawl
ENTRYPOINT ["slacrawl"]
CMD ["--help"]
