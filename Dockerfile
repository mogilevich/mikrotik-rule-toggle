FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY server/ ./server/
RUN go build -o /app-bin ./server/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app-bin /hook-server
COPY mikrotik/ /mikrotik/
COPY entrypoint.sh /entrypoint.sh
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
