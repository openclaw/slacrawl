# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26.6
ARG ALPINE_VERSION=3.23

FROM golang:${GO_VERSION}-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df AS build
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
