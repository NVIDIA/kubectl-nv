# The kubectl-nv agent contract

This page is what a program needs to know to drive `kubectl-nv`: what it
prints, what its exit codes mean, how to ask it for less, what it will never
do to a cluster, and how to reach it over the Model Context Protocol.

Everything here is a contract. A new meaning gets a new name or a new code
rather than a new reading of an existing one.

## Output

Every command prints one object to stdout, stamped with the API version and
the kind of this contract:

```json
{
  "apiVersion": "kubectl-nv.nvidia.com/v1alpha1",
  "kind": "GPUNodeList",
  "metadata": {},
  "items": []
}
```

The format is chosen with `-o`:

| `-o` | What it prints |
|---|---|
| `table` | Aligned columns, the default, for a person |
| `wide` | The table with the columns a kind marks as wide |
| `json` | The object above |
| `yaml` | The same object as YAML |
| `name` | One `kind/name` line per item |

`json` and `yaml` are the same object: the YAML is produced from the JSON
struct tags, so a field has one name in both.

A list always carries an `items` array. It is `[]` when there is nothing to
report, never `null`, so that a consumer can range over it without checking
first.

### Kinds

| Kind | What it describes |
|---|---|
| `GPUNode` | One GPU capable node |
| `GPUNodeList` | A list of them |
| `HealthReport` | The result of a health check run |
| `TopologyReport` | The device topology of a node or of the cluster |
| `BundleManifest` | The index of a must-gather bundle |
| `DebugReport` | One node's inventory next to the bundle collected for it |

### Schemas

Each kind has a JSON Schema generated from the Go type the command marshals,
so the schema cannot describe something `kubectl-nv` does not print.

```console
kubectl nv schema GPUNodeList     # by kind
kubectl nv schema list            # by the command that prints it
```

The same schemas are checked into `docs/schema/<kind>.json`, written by `make
schema`. A test fails when a checked-in file stops matching what the generator
produces, so the published contract cannot drift from the code.

In those schemas `apiVersion` and `kind` are required and pinned to one value
each, and `metadata` is trimmed to the five fields anything here sets: `name`,
`namespace`, `labels`, `annotations` and `creationTimestamp`. The same five are
the `metadata` paths `--fields` accepts, so the projection and the schema
describe one object.

A timestamp is an RFC3339 string or `null`. The null is not a choice: a
timestamp comes from `metav1.Time`, which writes `null` for its zero value, so
a node condition the cluster reported without a heartbeat and the
`creationTimestamp` of an object built in memory are both nulls on the wire. A
schema saying `string` there would be a contract this tool does not satisfy.

The schemas describe what a command prints whole. Output projected with
`--fields` is not expected to satisfy them: a projection keeps `apiVersion`,
`kind` and `metadata.name` and drops whatever else was not asked for, including
fields the schema marks `required`. Validate the unprojected output.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The run completed and found nothing at or above `--fail-on` |
| 1 | The run found something at or above `--fail-on` |
| 2 | The command line or the configuration was wrong |
| 3 | The run completed over less than it was asked for |
| 4 | The run could not happen |

Findings win over partial results. A run that found something at or above the
threshold exits 1 even when part of the cluster stayed out of reach, because
that is what the caller has to hear first. A run that found nothing but could
not see everything exits 3, so that "clean" is never reported for a scan that
did not happen; what was missed is listed alongside the findings, under
`unreachable`.

Where that list sits depends on the kind, because each one puts its status
where its own shape puts it. A `HealthReport` embeds its status inline, so the
paths are `findings`, `partial` and `unreachable` at the top level. A
`TopologyReport` and a `DebugReport` carry theirs in a field, so the paths are
`status.findings`, `status.partial` and `status.unreachable`. Run `kubectl nv
schema <kind>` rather than guessing.

Exit 4 means nothing was checked. No conclusion can be drawn from it.

`--fail-on` takes `info`, `warning` or `error` and defaults to `error`. A
value that is none of those is exit 2 rather than a silent fallback: the
difference between a gate and no gate must not be a typo.

## Streams

stdout carries the output object and nothing else. Progress, warnings and
anything else meant for a person go to stderr, so a pipe reading stdout never
has to filter.

No command ever prompts. There is nothing to answer, nothing to confirm, and
no terminal is required: a command that asked a question would hang in a pipe.

Colour is used only when stderr is a terminal, `NO_COLOR` is unset and
`--no-color` was not given. Control characters are stripped from anything that
came from the cluster before it reaches stderr, so a node name cannot repaint
the screen.

## Bounded output

Two flags are on every list-like command, and they mean the same thing on all
of them.

`--limit N` keeps the first N items after the command's own sort. Truncation
is reported twice: a note on stderr, and a remaining count in the output,
because a program reading stdout never sees stderr.

Where that count sits depends on the kind, because each one puts its status
where its own shape puts it:

| Kind | Where the remaining count is |
|---|---|
| `GPUNodeList` | `metadata.remainingItemCount` |
| `HealthReport` | `remainingItemCount`, at the top level |
| `TopologyReport` | `status.remainingItemCount` |
| `GPUNode`, `DebugReport` | nowhere: `--limit` is exit 2 on them |

A report that was not truncated carries no such key at all, and the stderr note
is not printed either. A zero would be true and would invite a reader to branch
on the value rather than on its presence.

What N counts is the repeated part of the kind: the items of a `GPUNodeList`,
the findings of a `HealthReport`, and the groups of a `TopologyReport` counted
across its levels, which are ordered narrowest first so that a limit keeps the
tightest groups. A `HealthReport`'s summary is left whole whatever the limit
is: it is the arithmetic over what was checked, and one that counted what was
printed would say the cluster is in better shape than it is.

A kind that is one object, `GPUNode` and `DebugReport`, has nothing for
`--limit` to cut, so it is exit 2 rather than an answer that ignored the flag.

```console
$ kubectl nv list --limit 2 -o json
note: showing 2 of 3 items (--limit 2)
```

`--fields a,b,c` keeps only the named dotted JSON paths on each item, for
instance `capacity` or `metadata.labels` on a `GPUNode`, or
`levels.groups.free` on a `TopologyReport`. `apiVersion`, `kind` and
`metadata.name` survive whatever is asked for: without them a consumer cannot
tell what it is holding or which thing the values belong to.

A path reaches inside a repeated part as well as into a nested object, so
`levels.groups.free` is one number of every group of every level, and the
arrays on the way down are kept in their own order. A section is a path too:
`levels` takes the levels whole, which is what a caller that wants a section
rather than a field asks for.

A path a kind does not have is exit 2, with the valid paths listed. A typo
would otherwise return an item missing the field the caller branches on, which
is worse than no answer.

Both flags work for every format. For `table` and `wide`, `--fields`
restricts the columns to the ones every path of which was asked for, always
keeping the name. A cell often renders several paths, such as a GPU count
showing requested out of allocatable out of capacity, and a column is kept only
when all of them were named: a table has nowhere to put half a cell, and a
column kept by one of its paths would print the others to a caller that named
neither. Asking for a section keeps the columns inside it. For `name` there is
nothing left to project.

A path the kind has and no column of the table renders is not an error: the
caller asked for something real, and `-o json` would give it. A note on stderr
says so, naming the paths, because a table that came back without the column
they asked for and said nothing reads as a table with no such field.

Run `kubectl nv schema <kind>` to see which paths a kind offers.

## Read-only guarantee

`kubectl-nv` never creates, updates, patches or deletes a Kubernetes object.
It is a diagnostic tool run against a cluster that is already unhappy, and a
write from it would change the state being reported.

This is enforced, not promised. A test executes every leaf of the command tree
against clients that fail the run on any write verb, and against an HTTP
server standing in for the API that fails on any method other than GET. A
command added without a fixture fails the test rather than being skipped.

### The one exec

`adm must-gather` runs `nvidia-bug-report.sh` inside a driver pod and copies
the result out. That is the only cluster-side action anywhere in the tool, and
it is confined to `must-gather` and `debug`.

It reads, but it is not without trace: the script writes
`/tmp/nvidia-bug-report.log.gz` inside the pod and nothing removes it
afterwards, so a gather leaves that file behind on each driver pod it
collected from.

It is turned off with `--no-exec`, which collects everything else and records
in the bundle what was skipped. A cluster where `pods/exec` is not allowed
degrades the same way and exits 3.

`kubectl nv debug` runs the same gather and takes the same `--no-exec`, so the
server serves it with the flag forced on. `adm must-gather` has no `--no-exec`
of its own yet, and until it does the server refuses to serve it at all rather
than serving it with the exec still available, and says so in the tool error.

Over MCP the exec is never on. See below.

## MCP

`kubectl nv mcp` serves the read-only commands as a Model Context Protocol
server on stdin and stdout. There is no listening socket: a port would be a
way into the cluster that outlives the process that asked for it.

```console
kubectl nv mcp                 # serve on stdio
kubectl nv mcp --list-tools    # print what it would offer, and exit
```

One tool per read-only leaf command, named by the command path with
underscores, for instance `adm_must-gather`. The list is derived from the
command tree, so a command added to `kubectl-nv` becomes a tool without the
server changing. Four commands are never tools: `mcp`, `help`, `completion`
and `version`.

Each tool's input schema is built from that command's flags and, when the
command documents any, its positional arguments. A property the schema does
not offer is refused rather than ignored, and a positional argument that would
be read as a flag is refused by name.

Calls run in process with `-o json`. The result carries the text the command
printed and, when that text is a JSON object or array, the same value as
structured content, so a caller does not parse it twice. A command that fails
comes back as a tool error carrying what it said, not as a protocol error the
model would never see.

### Which flags a tool offers

The input schema is an allowlist, not a filter. A tool offers exactly:

- the leaf command's own non-persistent flags, and
- `--namespace` (`-n`), the one inherited flag that asks about the request
  rather than about the connection.

Nothing else. In particular none of `--kubeconfig`, `--context`, `--cluster`,
`--user`, `--server`, `--token`, `--as`, `--as-group`, `--as-uid`,
`--as-user-extra`, `--certificate-authority`, `--client-certificate`,
`--client-key`, `--insecure-skip-tls-verify`, `--tls-server-name`,
`--proxy-url`, `--cache-dir`, `--request-timeout`, `--disable-compression` or
`--debug` is reachable from a tool call.

The reason is that the read-only guarantee says nothing about *which* cluster
is being read. A model that could set `--server` or `--token` could point
`kubectl-nv` at a cluster, a proxy or an identity nobody granted it, and every
call would still be a read. Those settings come from the flags given to
`kubectl nv mcp` itself, are parsed once, and are handed to every call whole.

Three more are left out for their own reasons:

- `--output`, because the server forces `-o json`: structured content is the
  JSON the command printed.
- `--no-exec`, covered below.
- any flag a command marks as writing on this machine. `schema --write-dir` is
  left out entirely; a directory flag such as `must-gather --artifacts-dir` is
  pinned by the server to a fresh directory, mode 0700, under the OS temp
  directory, a different one per call, and the path is reported in the result.

A property the schema does not offer is refused rather than ignored, and a
positional argument that would be read as a flag is refused by name. A refusal
names only the flag, never its value: echoing a rejected `--token` back would
copy the secret into the model's context.

### Exec over MCP

A command that can run something inside a pod carries the annotation
`kubectl-nv.nvidia.com/exec: "true"`. The server reads that annotation, never
a list of names: a command renamed, or a new one that execs, stays gated
without the server being edited.

A gated command is served with `--no-exec` forced on, and is not served at all
while that flag does not exist. Nothing a model decides should end up
executing inside a workload, however the request was phrased.

Three things enforce it rather than one: the flag is left out of the input
schema, a property the schema does not offer is refused, and the forced flag
is appended last so that it wins over anything earlier on the command line. A
command that can exec and has no `--no-exec` flag is refused outright rather
than run.

A gated command also writes files on this machine, so its tool is announced
with `readOnlyHint: false`. The hint is about the whole environment, not only
the cluster.

### Choosing the library

Two Go implementations were compared with read-only `gh api` calls on
2026-10-11:

| Library | Stars | Latest release | Last commit |
|---|---|---|---|
| `modelcontextprotocol/go-sdk` | 5198 | v1.8.0, 2026-09-14 | 2026-10-10 |
| `mark3labs/mcp-go` | 9154 | v1.2.1, 2026-10-09 | 2026-10-09 |

Both are active and neither is archived. `mcp-go` has the larger following
and is what `dims/kubectl-mcp` uses.

The official SDK was chosen anyway, for three reasons. It is the
implementation the specification is written against, so a protocol revision
lands in it first rather than being tracked afterwards. It reached v1.0.0 on
2025-09-30, against `mcp-go`'s 2026-09-02, which is a longer run of API
stability to point at. And it ships an in-memory transport, which lets a tool
call be tested over the real protocol without a process boundary; that is what
the exec gate tests use.

## Commands

Every leaf of the tree, the kind it prints, and which of the three output
flags apply to it. "List-like" is what `--limit` counts: a command that has
one takes `--limit`, and one that does not answers it with exit 2.

| Command | Kind | List-like | `-o` | `--fields` | `--limit` |
|---|---|---|---|---|---|
| `list` | `GPUNodeList` | Yes, the nodes | Yes | Yes | Yes |
| `info NODE` | `GPUNode` | No, one node | Yes | Yes | Exit 2 |
| `health` | `HealthReport` | Yes, the findings | Yes | Yes | Yes |
| `topology` | `TopologyReport` | Yes, the groups | Yes | Yes | Yes |
| `debug NODE` | `DebugReport` | No, one node | Yes | Yes | Exit 2 |
| `adm must-gather` | `BundleManifest` | A directory, not an object | No | No | No |
| `explain CODE` | None; Markdown | No | No | No | No |
| `schema [KIND]` | None; a JSON Schema | No | No | No | No |
| `mcp` | None; serves the protocol | No | No | No | No |
| `version` | None; one line | No | No | No | No |

The kind column is also what `kubectl nv schema <command>` answers with, so a
caller can ask for a schema by the command it was about to run rather than by a
kind it has to look up. `explain` has no kind and says so, with exit 2: it
prints a Markdown catalog entry, not an object of this contract.

`explain`, `schema`, `mcp` and `version` read nothing from a cluster and need
no kubeconfig.

`adm must-gather` and `debug` are the two commands that write on the machine
`kubectl-nv` runs on. `must-gather` prints no object at all: it writes a
directory and reports progress on stderr, and the `BundleManifest` kind is the
`manifest.json` inside that directory. `debug` writes the same bundle and
prints a `DebugReport` about it.

Both take `--redact`, which rewrites the node names, hostnames, addresses and
credential values out of the bundle before it is indexed. The mapping is keyed
per run and never written, so a node keeps one pseudonym throughout a bundle
and takes a different one in the next. `docs/must-gather.md` lists what is and
is not covered.

### Exit codes over MCP

Every tool result carries the code the same command would have left the
process with, under `kubectl-nv.nvidia.com/exit-code` in `_meta`, and states it
in the text.

| Exit code | `isError` | What the result carries |
|---|---|---|
| 0 | false | The report, as text and as structured content |
| 1 | false | The report; findings at or above `--fail-on` |
| 3 | false | The report; part of the cluster was out of reach |
| 2 | true | The usage error; there is no report |
| 4 | true | The failure; nothing was checked |

Exit 1 and 3 are results, not errors. A command that found something has
answered the question, and a caller that treated that as a failed call would
throw away the findings it asked for.
