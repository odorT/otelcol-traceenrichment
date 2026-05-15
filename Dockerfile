FROM golang:1.26 AS builder

WORKDIR /src

# Install OCB at the pinned version
RUN go install go.opentelemetry.io/collector/cmd/builder@v0.152.0

# Copy module files first so dependency layer is cached
COPY go.mod go.sum ./
RUN go mod download

# Copy source tree
COPY . .

# Build the custom collector binary via OCB
RUN builder --config builder-config.yaml --skip-strict-versioning

# --- Final stage ---
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /src/build/otelcol-traceenrich /otelcol-traceenrich

EXPOSE 4317 4318 8888 8889 13133 55679

ENTRYPOINT ["/otelcol-traceenrich"]
CMD ["--config", "/conf/collector.yaml"]
