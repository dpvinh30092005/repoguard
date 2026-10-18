# syntax=docker/dockerfile:1
# repoguard service WITH the executor: needs the docker CLI and git.
# Run it next to a Docker daemon (see docker-compose.executor.yml) — ideally a
# dedicated host/VM, because access to the Docker socket is root on that host.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/repoguard ./cmd/repoguard

FROM docker:27-cli
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/repoguard /usr/local/bin/repoguard
RUN mkdir -p /data
ENV REPOGUARD_DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["repoguard"]
CMD ["serve"]
