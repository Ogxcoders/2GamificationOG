# control-plane (Go) — multi-stage build.
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY services/control-plane/go.mod services/control-plane/go.sum ./
RUN go mod download
COPY services/control-plane ./
RUN CGO_ENABLED=0 go build -o /out/control-plane ./cmd/control-plane

FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget
COPY --from=builder /out/control-plane /usr/local/bin/control-plane
ENV CONTROL_PLANE_BIND=0.0.0.0:8080
EXPOSE 8080
USER 1000
ENTRYPOINT ["control-plane"]
