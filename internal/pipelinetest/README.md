# Pipeline tests

A pipeline test runs an Alloy configuration in process, sends it data, and
asserts on what comes out. It needs no network, no containers and no backend,
so a test runs in well under a second once its data arrives.

Pipeline tests run in two ways:

- `go test ./internal/pipelinetest/`, which runs every `tests/*/test.yaml`.
  This is how Alloy tests itself.
- `alloy test`, which runs any test file. **Experimental**: the command, the
  file format and the JSON output may change in any release. Design decisions
  and how to revisit them are in [DECISIONS.md](DECISIONS.md).

## Run tests

```sh
alloy test                       # every test file under the current directory
alloy test ./tests/checkout.test.yaml ./tests/logs/
alloy test --output=json .       # one JSON report, for CI or an agent
alloy test --timeout=1m -v .     # retry assertions longer; show Alloy's logs
```

A directory is searched recursively for files named `test.yaml` or ending in
`.test.yaml`. The exit status is 0 when every test passes and 1 otherwise.

Text output marks each assertion with ✓ or ✗. For a failed test it also shows
what reached the sink, and any component that is not healthy, which usually
explains the failure. A test that cannot run, for example because its config
does not load, is reported as an error. The error names the line in your own
config file.

## Test a real configuration

Test the config you deploy, unchanged. The test file names the config and
swaps its edges for the harness:

1. **Point at the config.** Set `config.path`, relative to the test file.
2. **Swap the exits.** List each `loki.write`, `prometheus.remote_write` and
   `otelcol.exporter.*` under `config.sinks`. Its data goes to the test sink,
   so no backend or credentials are needed.
3. **Swap the entries.** For each source, such as `loki.source.file` or
   `prometheus.scrape`, add an input with `replace: <component id>`. The
   input's data goes where the source sent it.
4. **Remove what needs a real environment**, such as `discovery.kubernetes`,
   under `config.remove`, along with anything that references it.
5. **Write inputs** that exercise the processing: a record each pipeline
   keeps, one it drops, one it rewrites.
6. **Assert** what should come out of each.
7. **Run `alloy test`** until it passes. A ✗ shows what reached the sink
   instead. An ERROR means the config did not load, and names the line.

```yaml
config:
  path: config.alloy
  sinks: [loki.write.cloud]
  remove: [discovery.kubernetes.pods, discovery.relabel.pods]

inputs:
  loki:
    - replace: loki.source.file.app
      entries:
        - {labels: {service_name: app}, line: 'level=info msg="served"'}
        - {labels: {service_name: app}, line: 'level=debug msg="cache miss"'}

assert:
  loki:
    - name: the level becomes a label
      match: {selector: '{service_name="app", level="info"}'}
    - name: debug lines are dropped
      count: 0
      match: {selector: '{level="debug"}'}
```

[tests/real-config](tests/real-config) does the same for metrics and traces.

What the test does not cover:

- The swapped components themselves. `external_labels` and relabel rules on
  an exit go with it, and a replaced `prometheus.scrape` no longer adds `job`
  and `instance`, so set them on the samples if the pipeline uses them.
- Which exit received the data. All exits share one sink.
- Components inside modules. Only top-level ones can be swapped.

Paths inside the config, such as `import.file` filenames, are relative to the
directory `alloy test` runs in, as for `alloy run`.

### With an agent

An agent can do steps 2 to 7. A prompt that works:

> Write an `alloy test` file for `config.alloy`, using the format in
> `internal/pipelinetest/README.md` of the Alloy repository. Send a few
> inputs per pipeline that each exercise its processing, and assert on the
> result. Run `alloy test --output=json` until it passes. If a failure looks
> like a bug in the config, say so instead of changing the assertion.

## File format

A test file has five sections. Only `config` and `assert` are required.

```yaml
flags: {}        # Alloy run flags, such as feature.loki.pipeline-v2.enabled: true
config: ...      # the Alloy configuration under test
mocks: {}        # stub upstreams the config can call
inputs: {}       # data to send into the pipeline
assert: {}       # what must reach the sink
```

Unknown fields are rejected, so a typo fails the test instead of being
ignored.

### `config`

Inline Alloy source:

```yaml
config: |
  loki.process "default" {
    forward_to = [pipelinetest.loki.receiver]
  }
```

or a file with the components to swap, as shown above. Inline configs send to
the sink through these references:

| Reference                           | Use as                                 |
| ----------------------------------- | -------------------------------------- |
| `pipelinetest.loki.receiver`        | a `forward_to` entry for logs          |
| `pipelinetest.loki.url`             | a `loki.write` endpoint URL            |
| `pipelinetest.prometheus.receiver`  | a `forward_to` entry for metrics       |
| `pipelinetest.prometheus.url`       | a `prometheus.remote_write` URL        |
| `pipelinetest.otel.input`           | an `output` entry for OTLP traces      |

### `inputs`

Each input sends to the receivers in `components`, or replaces an entry
component with `replace`. Set one of them.

```yaml
inputs:
  loki:
    - components: [loki.process.default.receiver]
      entries:
        - labels: {service_name: api}
          line: 'level=error msg="boom"'
          timestamp: 2026-04-22T12:00:00Z       # optional, defaults to now
          structured_metadata: {trace_id: abc}  # optional
  prometheus:
    - components: [prometheus.relabel.default.receiver]
      samples:
        - series: 'http_requests_total{method="post"}'   # or labels: {__name__: ..., ...}
          value: 1027
          type: counter                                  # optional metadata: type, help, unit
  traces:
    - components: [otelcol.processor.batch.default.input]
      spans:
        - name: GET /checkout
          service_name: shop              # the service.name resource attribute
          kind: server                    # internal (default), server, client, producer, consumer
          status: error                   # unset (default), ok, error
          attributes: {http.route: /checkout}
          resource_attributes: {k8s.namespace.name: shop}
```

To test components that receive data on their own listener, send to it
directly with `http`, `tcp` or `udp` inputs. See `HTTPInputSchema`,
`TCPInputSchema` and `UDPInputSchema` in the Go source, and the
`loki-source-*` tests for examples.

### `mocks`

`mocks.http` starts stub HTTP servers with canned responses, for components
that pull from an API or probe an endpoint. Reference one from the config as
`pipelinetest.mocks.http.<name>.url`. See `prometheus-exporter-blackbox`.

### `assert`

Assertions are grouped by signal: `loki`, `prometheus` and `traces`. Each
assertion is one of two types:

- `contains` passes when at least one item matches.
- `count` passes when exactly `count` items match. With no `match`, it counts
  everything of that signal.

`type` can be left out: an assertion with `count` is a count assertion, and
any other is `contains`. `name` is optional and shows in reports.

Assertions are retried until they all pass at once, or the timeout expires.
So `count: 0` can pass before the data it rules out arrives. Pair it with a
`contains` assertion on data sent alongside it.

**Logs** match on `selector`, `labels`, `line`, `timestamp` and
`structured_metadata`:

```yaml
- match:
    selector: '{service_name="api", level=~"warn|error"}'   # the entry may carry other labels
    line: 'boom'
```

**Metrics** match on `selector`, `labels`, `value`, `timestamp`, `histogram`
and `metadata` (`type`, `unit`, `help`):

```yaml
- match:
    selector: 'probe_success{job="blackbox"}'
    value: 1
```

**Traces** match on `name`, `service_name`, `kind`, `status`, `scope`,
`attributes` and `resource_attributes`.

`selector` is a PromQL series selector, with the `=`, `!=`, `=~` and `!~`
operators. It is the shortest way to match, and the series may carry labels it
does not name. `labels`, `structured_metadata` and the trace attribute maps
take a plain map, or a mode and values:

```yaml
labels: {level: error, service_name: api}     # default mode
labels:
  mode: partial                                # exact or partial
  values: {level: error}
```

The default mode is `exact` for log and metric labels, and `partial` for span
and resource attributes, because SDKs and processors add attributes of their
own.

### Not supported yet

- Profiles: `pyroscope.write` cannot be replaced with the sink, and there are
  no profile inputs or assertions.
- OTLP metrics and logs reach the sink and are counted in the report, but
  cannot be asserted on. Convert them to Prometheus or Loki in the config to
  test them today.
- Swapping components inside modules.
