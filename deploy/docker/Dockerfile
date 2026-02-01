# syntax=docker/dockerfile:1
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION} AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGET
RUN test -n "$TARGET"
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o /out/app ./${TARGET}

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
