# syntax=docker/dockerfile:1
FROM golang:1.26-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS builder
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

FROM alpine:3.23@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 1000 lfsd \
    && adduser -S -D -H -u 1000 -G lfsd -s /sbin/nologin lfsd
COPY --from=builder /out/lfsd /app/lfsd
ENV FLAMEGO_ENV=production
EXPOSE 3356
USER lfsd:lfsd
ENTRYPOINT ["/app/lfsd"]
