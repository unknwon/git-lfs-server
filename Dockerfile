# syntax=docker/dockerfile:1
FROM golang:1.26.10-alpine3.24@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS builder
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

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates "zlib>=1.4-r0" \
    && addgroup -S -g 1000 lfsd \
    && adduser -S -D -H -u 1000 -G lfsd -s /sbin/nologin lfsd
COPY --from=builder /out/lfsd /app/lfsd
ENV FLAMEGO_ENV=production
EXPOSE 3356
USER lfsd:lfsd
ENTRYPOINT ["/app/lfsd"]
