# syntax=docker/dockerfile:1.27

ARG GO_VERSION=1.27.3
ARG ALPINE_VERSION=3.24

FROM golang:${GO_VERSION}-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS build
RUN apk add --no-cache --upgrade 'ca-certificates>=20261020-r0' 'ca-certificates-bundle>=20261020-r0'
ARG GO_VERSION
RUN test "$(go env GOVERSION)" = "go${GO_VERSION}" \
    && apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/openclaw/slacrawl/internal/cli.version=${VERSION}" -o /out/slacrawl ./cmd/slacrawl

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache --upgrade 'ca-certificates>=20261020-r0' 'ca-certificates-bundle>=20261020-r0'
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
