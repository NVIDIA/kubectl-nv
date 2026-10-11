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
each, a timestamp is an RFC3339 string, and `metadata` is trimmed to the five
fields anything here sets: `name`, `namespace`, `labels`, `annotations` and
`creationTimestamp`. The same five are the `metadata` paths `--fields`
accepts, so the projection and the schema describe one object.

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
did not happen; what was missed is listed in `status.unreachable`.

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
is reported twice: a note on stderr, and `metadata.remainingItemCount` in the
output, because a program reading stdout never sees stderr.

```console
$ kubectl nv list --limit 2 -o json
note: showing 2 of 3 items (--limit 2)
```

`--fields a,b,c` keeps only the named dotted JSON paths on each item, for
instance `status.ready` or `metadata.labels`. `apiVersion`, `kind` and
`metadata.name` survive whatever is asked for: without them a consumer cannot
tell what it is holding or which thing the values belong to.

A path a kind does not have is exit 2, with the valid paths listed. A typo
would otherwise return an item missing the field the caller branches on, which
is worse than no answer.

Both flags work for every format. For `table` and `wide`, `--fields`
restricts the columns to the ones that render a requested path, always keeping
the name. For `name` there is nothing left to project.

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
the result out. That is the only cluster-side action anywhere in the tool, it
is confined to `must-gather` and `debug`, and it reads: nothing is left behind
in the pod.

It is turned off with `--no-exec`, which collects everything else and records
in the bundle what was skipped. A cluster where `pods/exec` is not allowed
degrades the same way and exits 3.

The flag itself is not in the tree yet. Until it lands, the MCP server refuses
to serve `must-gather` at all rather than serving it with the exec still
available, and says so in the tool error.

Over MCP it is never on. See below.

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

Two flags are not offered and cannot be set: `--output`, because the server
needs JSON to build structured content, and `--no-exec`, because of the next
paragraph.

### Exec over MCP

`must-gather` and `debug` are served with `--no-exec` forced on, and are not
served at all while that flag does not exist. Nothing a model decides should
end up executing inside a workload, however the request was phrased.

Three things enforce it rather than one: the flag is left out of the input
schema, a property the schema does not offer is refused, and the forced flag
is appended last so that it wins over anything earlier on the command line. A
command that can exec and has no `--no-exec` flag is refused outright rather
than run.

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
lands in it first rather than being tracked afterwards. It has been past 1.0
since 2026-07 against `mcp-go`'s 2026-09, which is a longer run of API
stability to point at. And it ships an in-memory transport, which lets a tool
call be tested over the real protocol without a process boundary; that is what
the exec gate tests use.

## Commands

The leaves below are what the tree carries today. Later tracks extend this
section as they land.

| Command | Prints | Kind |
|---|---|---|
| `adm must-gather` | A support bundle on disk | `BundleManifest` |
| `schema` | The JSON Schema of a kind | n/a |
| `mcp` | Nothing; serves the protocol | n/a |
| `version` | The version string | n/a |

`schema`, `mcp` and `version` read nothing from a cluster and need no
kubeconfig.
