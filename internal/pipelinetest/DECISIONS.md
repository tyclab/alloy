# Pipeline tests as a user-facing feature: decisions

This records the choices made for the first, experimental version of
`alloy test`. Each states what was chosen, why, and how to change it. Nothing
here is hard to reverse yet: the command is marked experimental, and nothing
outside this package depends on the format.

The user guide is [README.md](README.md).

## Scope of the first version

In:

- `alloy test` with text and JSON output.
- Testing a real config file unchanged, by swapping its entry and exit
  components for the harness, and removing components that need a real
  environment.
- Logs and metrics, as before, plus OTLP traces: span inputs, a traces sink,
  and span assertions. Metrics gain a sample input, so `prometheus.scrape` can
  be replaced.
- Shorter assertions: `selector`, an inferred `type`, plain maps for labels,
  and an optional `name`.
- An example real config tested end to end, in [tests/real-config](tests/real-config).

Out, and the likely next steps:

1. **Profiles.** Add a `pyroscope.write` sink target, a profile input, and
   assertions on profile labels and sample types.
2. **OTLP metrics and logs.** The sink counts them but cannot assert on them.
   Either flatten them like spans, or convert them to the Prometheus and Loki
   forms the assertions already understand.
3. **`alloy test init config.alloy`.** Generate the test skeleton
   deterministically: the sinks and replaced inputs follow from component
   names, as the tables below show. That leaves only the data and the
   assertions to a human or an agent.
4. **Swapping components inside modules.**
5. **`hold_for`**, so a `count` must keep passing for a while. See "Retrying
   assertions" below.
6. **A version field in the JSON report**, before the format is called stable.

## The command

**Chosen:** a top-level `alloy test [path ...]`. It is labelled experimental
in its help, and is not gated behind `--stability.level`.

**Why:** users run their configs with `alloy run` and check them with
`alloy validate`, so `alloy test` is where they will look. A stability flag on
a command that changes nothing at runtime adds friction without protecting
anything.

**To change:** the command is a thin wrapper in
`internal/alloycli/cmd_pipelinetest.go` around `pipelinetest.RunFiles`.
Renaming it, or moving it under `alloy tools`, touches that one file.

## The harness ships in the binary

**Chosen:** the `pipelinetest.*` components are part of the `alloy` binary.
They are registered on first use by `harness.Register`, not in `init`, so they
only exist in a process that runs tests. `alloy run` cannot use them, and the
component list the OTel engine checks is unchanged.

**Why:** a separate binary is one more thing to install and to keep at the
same version as Alloy. Tests must run the exact components the user runs.

**To change:** if binary size or the dependency surface becomes a concern,
build `alloy test` into a separate binary from the same package. Nothing in
the package assumes it is linked into `alloy`.

## Swapping components in a real config

**Chosen:** the test names a config file and lists what to swap:

- `config.sinks`: exit components, replaced by the sink.
- `inputs.<signal>[].replace`: entry components, replaced by that input.
- `config.remove`: components deleted outright.

The swap edits the config's source text, using positions from the parsed
syntax tree. Removed blocks become as many blank lines as they spanned, and
everything generated goes after the config. So comments and formatting
survive, and every line number in a load error still points at the user's
file.

**Why:** writing the swaps into the test, not the config, means the config
under test is byte for byte the one deployed. Putting `replace` on the input
keeps each entry component next to the data that stands in for it.

**Alternatives:** one `config.replace: {component: sink | input}` map. It is
one place to look, but then inputs need a second way to say which replacement
they feed. Printing a modified syntax tree is simpler code, but loses
comments and shifts line numbers.

**To change:** the schema lives in `ConfigSchema` and the input schemas, and
the logic in `config_transform.go`. Only top-level blocks are swapped.
Supporting modules means searching `declare` bodies and resolving
`import.*` sources.

### Exit components

| Component                 | Reference rewritten      | Sink export                         |
| ------------------------- | ------------------------ | ----------------------------------- |
| `loki.write`              | `<id>.receiver`          | `pipelinetest.sink.out.loki_receiver` |
| `prometheus.remote_write` | `<id>.receiver`          | `pipelinetest.sink.out.prometheus_receiver` |
| `otelcol.exporter.*`      | `<id>.input`             | `pipelinetest.sink.out.otel_input`  |

Any other component is rejected with the list above. A reference to another
export of a sink component, such as an exporter's `.something`, is an error,
not a silent miss.

**Consequence:** whatever the exit component does itself is not tested:
`external_labels`, `write_relabel_config`, and the wire protocol.

**Alternative, for later:** keep the exit component and point its URL at the
sink, as inline tests already do with `pipelinetest.loki.url`. That tests
external labels and the protocol, but needs to know each component's URL
argument, and needs an OTLP endpoint in the sink. It could be an opt-in per
sink.

### Entry components

A replaced component's `forward_to` expression becomes the input's
destination. For OTel receivers it is `output.traces`. The expression is
copied as written, with any sink references in it rewritten.

**Consequence:** what the entry component does itself is not tested either.
In particular a replaced `prometheus.scrape` no longer adds `job` and
`instance`.

**Alternative, for metrics:** keep the scrape and point its `targets` at a
generated `prometheus.exporter.static`. That tests the scrape's own labels,
but needs a short scrape interval to be forced on it, and the test waits for
a scrape.

## Signals

**Traces** were added in this version, because OTLP pipelines are a large
part of real configs. Spans are flattened into one record each, with
attributes rendered as strings, so assertions do not walk the resource and
scope hierarchy.

Attribute maps on spans match **partially** by default, unlike labels on logs
and metrics, which match exactly as they did before. SDKs and processors add
attributes, so an exact default would make almost every trace assertion
verbose. The defaults are set per field in `buildSpanMatchers` and the Loki
and Prometheus builders.

**Metric inputs** send samples straight to the destination's appender, in one
transaction per destination. Each sample takes a series in text exposition
form, `name{label="value"}`, because that is how users and agents already
write series.

## Assertions

- **`selector`** is a PromQL series selector, parsed by Prometheus' own
  parser, for both metrics and logs. A LogQL stream selector has the same
  syntax. It replaced 6 lines with 1 for the common "this series exists with
  these labels" check. `prometheus-exporter-blackbox` was converted, from 269
  lines to 131.
- **`type` is inferred**: `count` if `count` is set, otherwise `contains`.
- **Plain maps** for labels: `labels: {job: api}`. A mapping is read as the
  full form only when its keys are `mode` and `values`, with `values` a
  mapping. The rare map of labels named `mode` and `values` must use the full
  form.
- **`name`** labels an assertion in reports. Without one, the report
  describes the match fields.

The old forms all still work. No test needed changing, apart from the
optional conversion above.

### Retrying assertions

All assertions are retried together until they all pass at once, or the
timeout expires (`--timeout`, default 30s). This is unchanged from the Go
harness. It means a `count: 0` assertion passes before the data it rules out
arrives. The README tells users to pair it with a `contains` on data sent
alongside. `hold_for`, listed above, would close the gap.

## Strictness

Test files are decoded with unknown fields rejected. This now applies to the
Go suite too, and every existing test passed without change. A typo in a
user-facing format otherwise fails silently: a misspelled `inputs` key shows
up only as "0 entries" after the 30s timeout.

## Defaults that changed

- Log input entries without a `timestamp` are stamped with the time they are
  sent, not the zero time. A real source never sends the zero time, and some
  stages drop old entries. No existing test depended on the old default.
- `config.path` is resolved against the test file's directory, not the
  working directory. No existing test used it.

Paths inside the Alloy config, such as `import.file` filenames, stay relative
to the working directory, as they are for `alloy run`. `alloy test` does not
change directory per test. Changing directory would make in-repo tests and
user tests behave differently from `alloy run`.

## Output

- **Text**, the default, streams one block per test file as it finishes:
  ✓ and ✗ per assertion, then for a failure the unhealthy components and up
  to 20 items per signal of what reached the sink. Color is on for a
  terminal, honours `NO_COLOR`, and can be forced with `--color`.
- **JSON** is one document at the end, `pipelinetest.Report`. A failed test
  carries the captured data, at most 100 items per signal, with totals. A
  passing test does not, which keeps reports small.
- The exit status is 1 for failures and for tests that could not run. CI only
  needs pass or fail; the report has the detail.

**Alternatives:** JSON Lines, streaming one object per test, suits long runs
better. JUnit XML would plug into CI dashboards. Both are additions next to
`WriteJSON` and `TextReporter` in `report.go`.

## Execution

Tests run one after another. The harness uses the default Prometheus registry,
because `prometheus.exporter.self` serves only that one. Running tests in
parallel needs a registry per runtime.

Each test gets a fresh temporary data directory, removed afterwards.
