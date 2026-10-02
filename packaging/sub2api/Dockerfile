FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=docker
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/oaiprism ./cmd/oaiprism

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 prism \
    && adduser -D -H -u 10001 -G prism prism \
    && mkdir -p /data && chown 10001:10001 /data
WORKDIR /app
COPY --from=build /out/oaiprism /usr/local/bin/oaiprism
# The dashboard is looked up at ./web/dist relative to the working directory.
COPY web/dist/ ./web/dist/
COPY packaging/sub2api/config.yaml ./config.yaml
ENV OAI_PRISM_CONFIG=/app/config.yaml
USER 10001:10001
EXPOSE 8787
VOLUME ["/data"]
CMD ["oaiprism", "serve"]
