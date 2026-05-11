# syntax=docker/dockerfile:1
FROM golang:1.26.3-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS builder
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
