# syntax=docker/dockerfile:1
# The webhook service: small, static, non-root.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/repoguard ./cmd/repoguard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/repoguard /repoguard
EXPOSE 8080
ENTRYPOINT ["/repoguard"]
CMD ["serve"]
