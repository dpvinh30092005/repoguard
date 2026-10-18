# syntax=docker/dockerfile:1
# Demo Jenkins for repoguard: plugins + configuration-as-code + the
# "repoguard-ci" job, with the same toolchains as the repoguard-ci image.
# Builds run on the controller here for simplicity; use agents in production.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/repoguard ./cmd/repoguard

FROM jenkins/jenkins:lts-jdk17
USER root
COPY --from=golang:1.24-bookworm /usr/local/go /usr/local/go
COPY --from=node:22-bookworm /usr/local/bin/node /usr/local/bin/node
COPY --from=node:22-bookworm /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl unzip gcc libc6-dev python3 python3-pip python3-venv \
 && ln -sf /usr/bin/python3 /usr/local/bin/python \
 && python3 -m pip install --no-cache-dir --break-system-packages ruff pytest \
 && npm install -g --no-fund --no-audit newman@6 \
 && rm -rf /var/lib/apt/lists/*
ARG SONAR_SCANNER_VERSION=6.2.1.4610
RUN curl -fsSL -o /tmp/sonar-scanner.zip \
      "https://binaries.sonarsource.com/Distribution/sonar-scanner-cli/sonar-scanner-cli-${SONAR_SCANNER_VERSION}-linux-x64.zip" \
 && unzip -q /tmp/sonar-scanner.zip -d /opt \
 && mv "/opt/sonar-scanner-${SONAR_SCANNER_VERSION}-linux-x64" /opt/sonar-scanner \
 && ln -s /opt/sonar-scanner/bin/sonar-scanner /usr/local/bin/sonar-scanner \
 && rm /tmp/sonar-scanner.zip
COPY --from=build /out/repoguard /usr/local/bin/repoguard
ENV PATH=/usr/local/go/bin:$PATH \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    GOTOOLCHAIN=auto \
    JAVA_OPTS="-Djenkins.install.runSetupWizard=false" \
    CASC_JENKINS_CONFIG=/usr/share/jenkins/ref/casc.yaml
USER jenkins
COPY deploy/stack/jenkins/plugins.txt /usr/share/jenkins/ref/plugins.txt
RUN jenkins-plugin-cli --plugin-file /usr/share/jenkins/ref/plugins.txt
COPY deploy/stack/jenkins/casc.yaml /usr/share/jenkins/ref/casc.yaml
COPY ci-templates/jenkins/Jenkinsfile /usr/share/jenkins/ref/repoguard/Jenkinsfile
