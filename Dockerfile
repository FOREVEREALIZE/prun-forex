# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/prun-forex . \
 && mkdir -p /out/data

# distroless/static has CA certs (needed for the Discord API) and runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/prun-forex /prun-forex
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV ADDR=:8080 DB_PATH=/data/forex.db
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/prun-forex"]
