# syntax=docker/dockerfile:1
# CI image for GitLab (or any runner): repoguard + Go + Node + Python + osv-scanner.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG OSV_SCANNER_VERSION=v2.7.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/repoguard ./cmd/repoguard
# osv-scanner needs a newer Go than this base image; GOTOOLCHAIN=auto fetches it for this one build.
RUN CGO_ENABLED=0 GOTOOLCHAIN=auto GOBIN=/out go install github.com/google/osv-scanner/v2/cmd/osv-scanner@${OSV_SCANNER_VERSION}

FROM node:22-bookworm
COPY --from=golang:1.24-bookworm /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-pip python3-venv git ca-certificates \
 && ln -sf /usr/bin/python3 /usr/local/bin/python \
 && python3 -m pip install --no-cache-dir --break-system-packages ruff pytest \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/repoguard /out/osv-scanner /usr/local/bin/
ENTRYPOINT []
CMD ["repoguard", "ci"]
