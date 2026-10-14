# syntax=docker/dockerfile:1.27

ARG GO_VERSION=1.27.2
ARG ALPINE_VERSION=3.24

FROM golang:${GO_VERSION}-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
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
