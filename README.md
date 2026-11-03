# k8s-logs-mcp

An MCP server that lets Claude (or any MCP client) read logs from **every pod of a Kubernetes
service** in one request: "show me the errors in `ms-auth` from the last hour" instead of
opening each pod in Freelens/OpenLens one by one.

It uses the same kubeconfig file as Freelens, OpenLens or `kubectl`. It only **reads** from the
cluster; the only thing it writes is downloaded log files on your own disk.

---

## Setup, step by step

### Step 1. Install the prerequisites

| What | Why | Check |
|---|---|---|
| Go 1.27 or newer | to build the server | `go version` |
| Claude Code | the MCP client | `claude --version` |
| `kubectl` (optional) | to check your access in step 3 | `kubectl version --client` |

If your kubeconfig signs in through a plugin (`aws`, `gke-gcloud-auth-plugin`, `kubelogin`),
that plugin must be on your `PATH` too, as it is for `kubectl`.

### Step 2. Build the server

```sh
git clone <this-repo-url> k8s-logs-mcp
cd k8s-logs-mcp
make build
```

This creates `bin/k8s-logs-mcp`. Note the **absolute** path to it; you need it in step 4:

```sh
echo "$(pwd)/bin/k8s-logs-mcp"
```

### Step 3. Find the kubeconfig for each cluster

Every cluster you added to Freelens/OpenLens by pasting a config is stored as a separate file:

| OS | Folder |
|---|---|
| macOS | `~/Library/Application Support/Freelens/kubeconfigs/` (or `.../OpenLens/kubeconfigs/`) |
| Linux | `~/.config/Freelens/kubeconfigs/` (or `~/.config/OpenLens/kubeconfigs/`) |
| Windows | `%APPDATA%\Freelens\kubeconfigs\` (or `%APPDATA%\OpenLens\kubeconfigs\`) |

You can also open the cluster in Freelens → **Settings → General**, which shows the path to its
kubeconfig.

The file names are random IDs. To see which file is which cluster (macOS example):

```sh
cd ~/Library/Application\ Support/Freelens/kubeconfigs
for f in *; do echo "== $f"; KUBECONFIG="$f" kubectl config get-contexts --no-headers; done
```

Write down the path for each environment you need (dev, test, stage, …).

> Clusters that came from `~/.kube/config` or a cloud CLI (`aws eks update-kubeconfig`,
> `gcloud container clusters get-credentials`) work the same way. Use that file's path.

### Step 4. Check that the credentials can read logs (optional, but saves time)

```sh
export KUBECONFIG="/path/to/your/kubeconfig"
kubectl auth can-i list pods     -n <namespace>
kubectl auth can-i get  pods/log -n <namespace>
```

Both must answer `yes`. Nothing else is required. If the role cannot read Services,
StatefulSets, DaemonSets or namespaces, the server skips them and tells you so (see
[How a service is found](#how-a-service-is-found)).

### Step 5. Register the server in Claude Code

Register **one server per environment**. The environment then shows in every tool name
(`mcp__k8s-logs-stage__get_service_logs`), so Claude cannot mix up stage and dev.

```sh
BIN=/absolute/path/to/k8s-logs-mcp/bin/k8s-logs-mcp
LOGS=/absolute/path/to/k8s-logs-mcp/logs

claude mcp add k8s-logs-dev   -s user -- "$BIN" --kubeconfig "/path/to/dev-kubeconfig"   --logs-dir "$LOGS/dev"
claude mcp add k8s-logs-test  -s user -- "$BIN" --kubeconfig "/path/to/test-kubeconfig"  --logs-dir "$LOGS/test"
claude mcp add k8s-logs-stage -s user -- "$BIN" --kubeconfig "/path/to/stage-kubeconfig" --logs-dir "$LOGS/stage"
```

- `-s user` makes the server available in **all** your projects. Use `-s project` to share it
  through the repo's `.mcp.json` instead.
- Quote paths that contain spaces (`Application Support`), as shown above.
- Always use absolute paths. Claude starts the server from whatever folder you are working in.
- Optional flags: `--namespace <ns>` to set a default namespace and `--context <name>` to pick a
  context when the file has several. See [Flags](#flags).

#### Alternative: edit the JSON by hand

Instead of running `claude mcp add`, you can add all environments at once. Open `~/.claude.json`,
find the top-level `"mcpServers"` object (create it if it is missing) and add one entry per
cluster:

```json
"mcpServers": {
  "k8s-logs-dev": {
    "type": "stdio",
    "command": "/absolute/path/to/k8s-logs-mcp/bin/k8s-logs-mcp",
    "args": [
      "--kubeconfig", "/Users/me/Library/Application Support/Freelens/kubeconfigs/<dev-file-id>",
      "--logs-dir", "/absolute/path/to/k8s-logs-mcp/logs/dev"
    ]
  },
  "k8s-logs-test": {
    "type": "stdio",
    "command": "/absolute/path/to/k8s-logs-mcp/bin/k8s-logs-mcp",
    "args": [
      "--kubeconfig", "/Users/me/Library/Application Support/Freelens/kubeconfigs/<test-file-id>",
      "--logs-dir", "/absolute/path/to/k8s-logs-mcp/logs/test"
    ]
  },
  "k8s-logs-stage": {
    "type": "stdio",
    "command": "/absolute/path/to/k8s-logs-mcp/bin/k8s-logs-mcp",
    "args": [
      "--kubeconfig", "/Users/me/Library/Application Support/Freelens/kubeconfigs/<stage-file-id>",
      "--logs-dir", "/absolute/path/to/k8s-logs-mcp/logs/stage"
    ]
  }
}
```

- In JSON every argument is its own string, so paths with spaces need no extra quoting.
- Keep entries you already have in `mcpServers`; just add these next to them.
- Need another cluster later (for example `prod`)? Copy an entry, rename it and change its
  `--kubeconfig`.

To share one config with the whole team, put the same block in a `.mcp.json` at the root of the
project. Write `${K8S_LOGS_BIN}`, `${KUBECONFIG_DEV}` and so on instead of paths: Claude Code
expands environment variables in `.mcp.json`, so each person sets their own paths in their shell
profile.

### Step 6. Restart Claude Code and confirm the connection

Restart Claude Code (or open a new session), then run:

```sh
claude mcp list
```

Each `k8s-logs-*` entry should show `✓ Connected`. Inside Claude Code, `/mcp` shows the same.

### Step 7. Ask for logs

Ask in plain language, and **name the environment**: "on dev", "from stage". Claude then uses
the tools of that server (`k8s-logs-dev`, `k8s-logs-stage`, …). If you don't name one, it may ask
you or pick one itself.

| You ask | Claude calls |
|---|---|
| "What services are there on dev?" | `list_services` |
| "Show the pods of ms-auth on stage" | `list_service_pods` |
| "Show errors in ms-auth on dev in the last hour" | `get_service_logs` with `since: 1h`, `filter: (?i)error` |
| "Why does ms-calculations-worker keep restarting?" | `list_service_pods`, then `get_service_logs` with `previous: true` |
| "Download all logs of ms-accounting from test" | `download_service_logs` |

Downloaded logs are saved as:

```
<logs-dir>/<service>/<YYYYMMDD-HHMMSS>/<pod>_<container>.log
```

---

## Tools

| Tool | What it does |
|---|---|
| `list_contexts` | Contexts (clusters) in the kubeconfig. |
| `list_namespaces` | Namespaces of a context. If listing them is not allowed, returns the context's own namespace. |
| `list_services` | Everything you can pass as `service`: **apps** (pods grouped by their `app` label), Services, Deployments, StatefulSets and DaemonSets, plus warnings for anything the role cannot read. |
| `list_service_pods` | Every pod of a service: phase, readiness, restarts, node, age, containers. |
| `get_service_logs` | Logs of every pod and container of a service, returned in the chat. |
| `download_service_logs` | Complete logs of every pod and container, written to files. Returns the file paths. |

Every tool also accepts optional `kubeconfig`, `context` and `namespace` arguments that override
the server defaults for that one call.

### `get_service_logs` / `download_service_logs` options

| Option | Default | Meaning |
|---|---|---|
| `service` | required | Service, Deployment, StatefulSet, DaemonSet name, or `app` label value. |
| `label_selector` | none | Use this pod selector instead of looking up `service`, e.g. `app=api,version=green`. |
| `container` | all | Read only this container. |
| `include_init` | `false` | Also read init containers. |
| `since` | none | Only lines newer than this duration: `15m`, `2h`, `72h`. |
| `previous` | `false` | Logs of the previous container instance (after a crash or restart). |
| `timestamps` | `false` | Prefix each line with its timestamp. |
| `tail_lines` | `200` for get, all for download | Last N lines per container. `0` means everything. |
| `filter` | none | get only: keep lines matching this Go regexp, e.g. `(?i)error\|panic`. |
| `max_bytes_per_container` | `65536` | get only: keep at most this many bytes of the newest lines per container. |

`filter` runs **after** `tail_lines`. To search a longer period, combine it with `since`, or set
`tail_lines: 0`.

### How a service is found

The `service` name is resolved in this order:

1. A **Service** with that name. Its pod selector is used.
2. A **Deployment**, **StatefulSet** or **DaemonSet** with that name. Its pod selector is used.
3. Pods labelled **`app=<name>`** (or `app.kubernetes.io/name=<name>`).

Step 3 covers blue/green setups: `ms-accounting` finds the pods of both `ms-accounting-blue` and
`ms-accounting-green`, because they share `app=ms-accounting`. Workers usually carry their own
label (`ms-accounting-worker`), so you ask for them by that name.

Kinds the credentials may not read are skipped rather than failing the request.

---

## Flags

| Flag | Default | Description |
|---|---|---|
| `--kubeconfig` | `$KUBECONFIG`, then `~/.kube/config` | Kubeconfig file. |
| `--context` | the file's current-context | Default context. |
| `--namespace` | the context's namespace | Default namespace. |
| `--logs-dir` | `./logs` | Where downloads go. Use an absolute path. |
| `--transport` | `stdio` | `stdio` for Claude Code / Desktop / Cursor, or `http`. |
| `--addr` | `127.0.0.1:8080` | Listen address for `--transport http`. |
| `--version` | | Print the version and exit. |

### Other MCP clients

Claude Desktop, Cursor and a project `.mcp.json` use the same entry:

```json
{
  "mcpServers": {
    "k8s-logs-dev": {
      "command": "/absolute/path/to/bin/k8s-logs-mcp",
      "args": ["--kubeconfig", "/path/to/dev-kubeconfig", "--logs-dir", "/absolute/path/to/logs/dev"]
    }
  }
}
```

To run the server as a streamable HTTP endpoint:

```sh
bin/k8s-logs-mcp --transport http --addr 127.0.0.1:8080 --kubeconfig /path/to/kubeconfig
```

---

## Maintenance

| Task | Command |
|---|---|
| Update after `git pull` | `make build`, then restart Claude Code |
| See what is registered | `claude mcp list`, or `claude mcp get k8s-logs-dev` |
| Change a kubeconfig or flag | `claude mcp remove k8s-logs-dev -s user`, then add it again (step 5) |
| Remove the server | `claude mcp remove k8s-logs-dev -s user` |

Claude Code stores these entries in `~/.claude.json` under `mcpServers`. That file also holds
other secrets, such as tokens of other MCP servers, so don't share it whole.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `✗ Failed to connect` in `claude mcp list` | The binary path is wrong or not absolute. Run it by hand: `/abs/path/bin/k8s-logs-mcp --version`. |
| `load kubeconfig: ... no such file` | Wrong kubeconfig path, or a path with spaces was not quoted when adding. |
| `dial tcp ... i/o timeout` | The cluster is not reachable from this machine. Connect to the VPN and check with `kubectl get pods`. |
| `Unauthorized` | The token in the kubeconfig expired. Refresh it in Freelens or with your cloud CLI. |
| `executable ... not found` | The auth plugin the kubeconfig needs (`aws`, `kubelogin`, …) is not on `PATH`. |
| `no Service, Deployment ... with this name` | Ask "list services on dev" to see the exact names. The error also says which kinds could not be read. |
| `previous: true` returns `unable to retrieve container logs` | Kubernetes has already discarded the previous instance. Read the current logs instead; for a crashing pod they show the last crash. `kubectl logs --previous` gives the same error. |
| Tools don't show up in Claude | Restart Claude Code after adding or changing the server. |

## Security

- The server only uses `get`/`list` on pods, `pods/log`, Services, apps workloads and
  namespaces. A read-only role such as `view`, or just `pods` + `pods/log`, is enough.
- Logs can contain secrets and personal data, and everything a tool returns is sent to the
  model. Prefer narrow requests (`since`, `filter`) on sensitive environments.
- Downloaded logs stay in `--logs-dir`. The repo's `.gitignore` excludes `logs/` and kubeconfig
  files. Never commit them.

---

## Development

```
cmd/k8s-logs-mcp/   entry point: flags, stdio / streamable HTTP transport
internal/kube/      kubeconfig loading, service → pods resolution, reading and downloading logs
internal/tools/     MCP tool definitions, input/output schemas, handlers
```

```sh
make test    # go test -race ./...
make lint    # go vet + golangci-lint
make build   # bin/k8s-logs-mcp
```

Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) and
[client-go](https://github.com/kubernetes/client-go).
