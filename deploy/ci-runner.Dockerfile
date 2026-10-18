# syntax=docker/dockerfile:1
# CI image for GitLab, Jenkins or the executor: repoguard + Go + Node + Python +
# osv-scanner + Newman (Postman) + sonar-scanner.
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
# Throwaway CI container: let pip install into the system Python (PEP 668),
# and fetch newer Go toolchains when a project's go.mod asks for one.
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    GOTOOLCHAIN=auto
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-pip python3-venv git ca-certificates curl unzip \
 && ln -sf /usr/bin/python3 /usr/local/bin/python \
 && python3 -m pip install --no-cache-dir --break-system-packages ruff pytest \
 && rm -rf /var/lib/apt/lists/*
# API tests: Newman runs Postman collections.
RUN npm install -g --no-fund --no-audit newman@6 && npm cache clean --force
# Code quality: SonarScanner CLI (the linux-x64 build bundles its own JRE).
ARG SONAR_SCANNER_VERSION=6.2.1.4610
RUN curl -fsSL -o /tmp/sonar-scanner.zip \
      "https://binaries.sonarsource.com/Distribution/sonar-scanner-cli/sonar-scanner-cli-${SONAR_SCANNER_VERSION}-linux-x64.zip" \
 && unzip -q /tmp/sonar-scanner.zip -d /opt \
 && mv "/opt/sonar-scanner-${SONAR_SCANNER_VERSION}-linux-x64" /opt/sonar-scanner \
 && ln -s /opt/sonar-scanner/bin/sonar-scanner /usr/local/bin/sonar-scanner \
 && rm /tmp/sonar-scanner.zip
COPY --from=build /out/repoguard /out/osv-scanner /usr/local/bin/
ENTRYPOINT []
CMD ["repoguard", "ci"]
