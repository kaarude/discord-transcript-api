# syntax=docker/dockerfile:1
FROM golang:1.26.6-alpine AS build
ARG BUILD_DATE
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN go test ./... \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/transcript-api ./cmd/transcript-api \
 && mkdir -p /runtime/data /runtime/transcripts

FROM scratch
ARG BUILD_DATE
ENV BUILD_DATE=$BUILD_DATE APP_ENV=production
WORKDIR /app
COPY --from=build /out/transcript-api /usr/local/bin/transcript-api
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=1000:1000 /runtime/ /app/
USER 1000:1000
EXPOSE 3010
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/usr/local/bin/transcript-api", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/transcript-api"]
