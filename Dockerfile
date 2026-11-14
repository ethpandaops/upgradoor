FROM node:24-alpine AS web
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /app/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -tags=webui -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o /upgradoor ./cmd/upgradoor

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 upgradoor && \
    mkdir -p /data && chown upgradoor /data
COPY --from=builder /upgradoor /usr/local/bin/upgradoor
USER upgradoor
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["upgradoor"]
CMD ["controller"]
