# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BUILD_DATE
ARG BUILD_COMMIT
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X 'main.buildDate=${BUILD_DATE}' -X 'main.buildCommit=${BUILD_COMMIT}'" \
    -o /out/lfsd ./cmd/lfsd

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 1000 lfsd \
    && adduser -S -D -H -u 1000 -G lfsd -s /sbin/nologin lfsd
COPY --from=builder /out/lfsd /app/lfsd
ENV FLAMEGO_ENV=production
EXPOSE 3356
USER lfsd:lfsd
ENTRYPOINT ["/app/lfsd"]
