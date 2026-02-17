FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/silicon-proxy ./cmd/silicon-proxy

FROM alpine:3.21
RUN adduser -D -H appuser
WORKDIR /app

COPY --from=builder /out/silicon-proxy /usr/local/bin/silicon-proxy
COPY config.example.jsonc /app/config.jsonc

USER appuser
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/silicon-proxy", "-config", "/app/config.jsonc"]