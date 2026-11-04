# syntax=docker/dockerfile:1

# One image with both commands, /app/server and /app/worker (the default),
# plus the engine and WAD they load. The ECS task runs it twice, picking the
# command per container. See infra/.

# ---- build stage ----
# Go must match the version that built the engine for any games already in
# the namespace: a game only replays on the exact doom.wasm that recorded it,
# and a different toolchain makes a different doom.wasm.
FROM --platform=$BUILDPLATFORM golang:1.27.1 AS build
WORKDIR /src

# Cache module downloads separately from source for faster rebuilds.
COPY go.mod go.sum ./
COPY third_party/gore/go.mod third_party/gore/go.sum third_party/gore/
RUN go mod download

COPY . .

# doom.wasm is GOOS=wasip1, so it's the same whatever the target platform.
# `make wad` uses wad/doom1.wad from the build context if it's there, and
# otherwise downloads it.
RUN make wad engine

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-arm64} \
    go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/ ./cmd/server ./cmd/worker
RUN mkdir /out/data

# ---- runtime image ----
# Distroless static: no shell, no package manager, runs as nonroot (65532).
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /src/build/doom.wasm build/doom.wasm
COPY --from=build /src/wad/doom1.wad wad/doom1.wad
COPY --from=build /out/server /out/worker ./

# The leaderboard file, written by the worker and read by the server. On
# Fargate this is a volume shared by the task's two containers. ECS copies
# a VOLUME's contents and ownership into the volume, which otherwise
# would be owned by root and unwritable by nonroot.
COPY --from=build --chown=65532:65532 /out/data data
VOLUME ["/app/data"]

ENTRYPOINT ["/app/worker"]
