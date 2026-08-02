# syntax=docker/dockerfile:1.18

FROM golang:1.26.5-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

FROM build AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go test -race ./...

FROM build AS compile
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/capsule ./cmd/capsule
RUN mkdir -p /out/data && touch /out/data/.volume-seed

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=compile /out/capsule /capsule
COPY --from=compile --chown=65532:65532 /out/data/ /data/
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/capsule"]
