# otelcol-traceenrichment

Custom OpenTelemetry Collector distribution with the `traceenrichment` processor. The processor correlates Cilium Tetragon eBPF security events with W3C trace context by maintaining an in-memory span cache keyed on container ID. When a Tetragon log record arrives, the processor looks up the cache and writes `trace_id`, `span_id`, and `service.name` directly onto the log record.

Built as the headline implementation artefact for the dissertation:
*Integrating Security and Observability in Distributed Systems* — Kamran Karimov, Baku Higher Oil School, 2026.

---

## How it works

Two pipelines run inside the same collector instance and share a single cache object via the factory.

**Traces pipeline** — receives OTLP spans from application pods. For each span, reads `container.id` from the resource attributes (populated by `k8sattributesprocessor`) and stores `(trace_id, span_id, service.name, start_time)` in a per-container ring buffer. Spans are not mutated.

**Logs pipeline** — reads Tetragon JSON logs from the host filesystem via `filelogreceiver`. For each log record, iterates every top-level attribute map and navigates the configured subpath to extract the container ID. Looks that ID up in the cache; on a hit, writes `correlation_hit=true`, `trace_id`, `span_id`, and `service.name` onto the record. On a miss, writes `correlation_miss=true`.

Both pipelines must reference the **same processor name** in the collector config so they resolve to the same shared cache instance.

---

## Repository layout

```
.
├── processor/
│   └── traceenrichmentprocessor/   # Go source for the custom processor
│       ├── processor.go            # TracesProcessor (cache write) + LogsProcessor (enrichment)
│       ├── cache.go                # Thread-safe span cache with TTL eviction
│       ├── factory.go              # OTel Collector factory; shared-state management
│       ├── config.go               # Config struct and validation
│       ├── *_test.go               # Unit tests
│       └── testdata/config.yaml    # Test fixture
├── builder-config.yaml             # OCB manifest — declares all components in the distribution
├── go.mod / go.sum                 # Module definition for the processor package
├── Dockerfile                      # Two-stage build: OCB builder → distroless runtime
├── deployment/
│   ├── otelcol-cr.yaml             # OpenTelemetryCollector CR — unified pipeline (traceenrichment active)
│   ├── otelcol-cr-siloed.yaml      # OpenTelemetryCollector CR — siloed baseline (processor absent)
│   ├── rbac.yaml                   # ServiceAccount + ClusterRole for k8sattributesprocessor
│   ├── tracingpolicy-cryptominer.yaml  # Tetragon TracingPolicy: sys_execve kprobe (Scenario A)
│   └── tracingpolicy-ssrf.yaml        # Tetragon TracingPolicy: tcp_connect kprobe (Scenario B)
└── attack-app/
    ├── src/
    │   ├── index.js                # Express entry point
    │   ├── miner.js                # execFile() wrapper that spawns miner-stub
    │   ├── tracing.js              # OTel Node.js SDK bootstrap (auto-instrumentation)
    │   └── routes/trigger.js       # POST /trigger/cryptojack and POST /trigger/ssrf handlers
    ├── miner-stub.js               # Miner stub: opens TCP to mining-pool-svc, burns CPU 30 s
    ├── Dockerfile
    └── deployment/
        ├── attack-app.yaml         # Deployment + Service for the attack app
        ├── mining-pool.yaml        # Fake mining pool (busybox nc listener on port 3333)
        └── otel-env.yaml           # ConfigMap: OTLP endpoint, batch delay tuning
```

---

## Building

Install the OpenTelemetry Collector Builder (OCB):

```bash
go install go.opentelemetry.io/collector/cmd/builder@v0.152.0
```

Build the custom collector binary:

```bash
builder --config builder-config.yaml --skip-strict-versioning
./build/otelcol-traceenrich --version
```

Or build the container image:

```bash
docker build -t otelcol-traceenrich:local .
```

---

## Processor configuration

```yaml
traceenrichment:
  cache:
    ttl: 30s                 # How long a span stays eligible for matching
    max_entries: 100000      # Hard cap; oldest container entry evicted first
    cleanup_interval: 5s     # Background sweep cadence
  container_id_attribute: container.id              # Resource attribute from k8sattributesprocessor
  tetragon_container_id_subpath: process.pod.container.id  # Dot path inside each Tetragon event map
  enrichment_attributes: [trace_id, span_id, service.name]
  miss_marker_attribute: correlation_miss
  match_timestamp_skew: 500ms   # Symmetric tolerance for kernel/SDK clock drift
```

The processor strips `containerd://`, `docker://`, and `cri-o://` scheme prefixes before cache lookup, so all container runtime ID formats resolve to the same key.

---

## Deployment

The collector runs as a DaemonSet (one pod per node) managed by the OpenTelemetry Operator. It mounts the Tetragon log socket from the host.

Apply RBAC first, then the collector CR:

```bash
kubectl apply -f deployment/rbac.yaml
kubectl apply -f deployment/otelcol-cr.yaml        # unified pipeline
# or
kubectl apply -f deployment/otelcol-cr-siloed.yaml # siloed baseline
```

Apply Tetragon tracing policies:

```bash
kubectl apply -f deployment/tracingpolicy-cryptominer.yaml
kubectl apply -f deployment/tracingpolicy-ssrf.yaml
```

Switch between unified and siloed modes by deleting one CR and applying the other. The switchover takes less than 60 seconds.

---

## Attack scenarios

Two attack scenarios exercise the two Tetragon tracing policies.

**Scenario A — cryptojack** (`POST /trigger/cryptojack`)

The attack app calls `execFile()` to spawn `miner-stub` as a detached child process. Tetragon fires a `sys_execve` kprobe event while the parent HTTP span is still active in the OTel SDK. The processor correlates the kprobe event with the live span and writes the trace ID onto the Tetragon log record.

**Scenario B — SSRF** (`POST /trigger/ssrf`)

The attack app makes an outbound HTTP request to the Azure IMDS endpoint (`169.254.169.254`). Tetragon fires a `tcp_connect` kprobe event. The parent span is held open for 500 ms so it is in the cache before the Tetragon log arrives.

Deploy the attack app:

```bash
kubectl apply -f attack-app/deployment/otel-env.yaml
kubectl apply -f attack-app/deployment/mining-pool.yaml
kubectl apply -f attack-app/deployment/attack-app.yaml
```

Trigger a scenario:

```bash
kubectl exec -n boutique deploy/attack-app -- \
  curl -s -X POST http://localhost:3000/trigger/cryptojack

kubectl exec -n boutique deploy/attack-app -- \
  curl -s -X POST http://localhost:3000/trigger/ssrf
```

Verify enrichment in Loki:

```logql
{service_name="tetragon"} | json | correlation_hit="true"
```

---

## Self-metrics

The processor exposes the following metrics on the collector's telemetry port (default `0.0.0.0:8888`).

| Metric | Type | Labels |
|---|---|---|
| `traceenrichment_spans_observed_total` | Counter | — |
| `traceenrichment_spans_skipped_total` | Counter | `reason` |
| `traceenrichment_events_total` | Counter | `result` (`enriched`, `miss_no_active_span`, `no_container_id`) |
| `traceenrichment_cache_size` | Gauge | — |
| `traceenrichment_cache_evictions_total` | Counter | `cause` (`ttl`, `max_entries`) |
| `traceenrichment_lookup_duration_seconds` | Histogram | `result` |

Correlation rate:

```promql
traceenrichment_events_total{result="enriched"}
  / ignoring(result) sum without(result) (traceenrichment_events_total)
```

---

## Notes

- eBPF and Tetragon are Linux-only. The processor itself is cross-platform, but the filelog receiver path and the Tetragon tracing policies have no equivalent on other operating systems.
- The matching policy selects the most recently started span whose `StartTime` falls in `[logTime - TTL - skew, logTime + skew]`. This is a deliberate approximation; short-lived spans that complete before the Tetragon event is emitted may be missed.
- The `OTEL_BSP_SCHEDULE_DELAY` in `otel-env.yaml` is set to 100 ms (default 5000 ms). This is required to ensure spans reach the collector cache before the corresponding Tetragon log records arrive.
