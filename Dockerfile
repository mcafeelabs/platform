# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/svcreg ./cmd/svcreg \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/gitserver ./cmd/gitserver

# svcreg in the cluster: query service and NATS kv-sync. (CI runs the CLI
# from source; sandbox-expire needs git history.)
FROM gcr.io/distroless/static-debian12:nonroot AS svcreg
COPY --from=build /out/svcreg /usr/local/bin/svcreg
ENTRYPOINT ["svcreg"]

# gitserver: smart-HTTP git for local clusters (Argo CD and Kargo read and
# push a copy of the services repo).
FROM buildpack-deps:bookworm-scm AS gitserver
RUN git config --system --add safe.directory '*'
COPY --from=build /out/gitserver /usr/local/bin/gitserver
USER 65532:65532
ENTRYPOINT ["gitserver"]
