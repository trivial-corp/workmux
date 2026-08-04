# A container image for the case where you want workmux on a box without a Go
# toolchain — a homelab server, or managed alongside other services by compose.
#
# Read deploy/compose.yaml before using this. The mounts are not arbitrary: paths
# inside the container must match the host, because agent state records absolute
# directories and stacks started through the mounted docker socket run on the host
# daemon. Mount your repo at the same path it lives at, or nothing lines up.
FROM golang:1.23-alpine AS build
WORKDIR /src
# No dependencies to fetch: the server is the standard library and the frontend is
# embedded, so this builds offline and the layer cache is just the source.
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /workmux ./cmd/workmux

FROM alpine:3.20
# git is required. docker-cli is only used when a project has a stack, and talks
# to the host daemon through a mounted socket — there is no daemon in here.
# This image deliberately has no coding agent: it's your choice and your
# credentials. Bake one in (e.g. `RUN npm i -g @anthropic-ai/claude-code`, which
# also pulls in nodejs) or run with "agent": null in workmux.json.
RUN apk add --no-cache git docker-cli openssh-client ca-certificates tini
COPY --from=build /workmux /usr/local/bin/workmux
EXPOSE 4315
# tini reaps the subprocesses workmux spawns; without an init, a container full of
# finished git and docker processes accumulates zombies.
ENTRYPOINT ["/sbin/tini", "--", "workmux", "--host", "0.0.0.0"]
