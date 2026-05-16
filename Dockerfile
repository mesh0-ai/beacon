# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/beacon .

FROM scratch
COPY --from=build /out/beacon /beacon
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
USER 65532:65532
ENTRYPOINT ["/beacon"]
