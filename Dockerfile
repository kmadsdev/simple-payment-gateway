# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webhook-receiver ./examples/webhook-receiver

# ---- example webhook receiver (used by docker-compose) ----
FROM gcr.io/distroless/static-debian13:nonroot AS receiver
COPY --from=build /out/webhook-receiver /webhook-receiver
USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/webhook-receiver"]

# ---- gateway (default target) ----
FROM gcr.io/distroless/static-debian13:nonroot AS gateway
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=3 CMD ["/gateway", "healthcheck"]
ENTRYPOINT ["/gateway"]
CMD ["serve"]
