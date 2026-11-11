# syntax=docker/dockerfile:1
FROM golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY lfs/go.mod lfs/go.sum ./lfs/
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
