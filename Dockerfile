FROM node:24-alpine AS web
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /app/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -tags=webui -trimpath \
    -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
    -o /upgradoor-controller ./cmd/upgradoor-controller

# The base is pinned by digest: a release image's digest is signed with its binaries.
FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f AS runtime
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 upgradoor && \
    mkdir -p /data && chown upgradoor /data
USER upgradoor
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["upgradoor-controller"]
CMD ["run"]

# The release image (--target release): the reproducible controller binary of `make release`, the
# one the release manifest names, not a build of its own.
FROM runtime AS release
ARG TARGETARCH
COPY dist/upgradoor-controller-linux-${TARGETARCH} /usr/local/bin/upgradoor-controller

# Development images: built from source.
FROM runtime
COPY --from=builder /upgradoor-controller /usr/local/bin/upgradoor-controller
