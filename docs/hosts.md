# Hosts: herdr, herdr GPUI, Orca, Rex and external hosts

kelpie delivers updates through the program the agent runs in, the host. It ships with four built-in hosts and can run [external host binaries](#external-hosts) registered in your config:

| Host | Config | Workspace ID | Caller variable | On by default |
| --- | --- | --- | --- | --- |
| [herdr](https://herdr.dev), the terminal UI | `hosts.herdr` | a pane, such as `w1:p2` | `$HERDR_PANE_ID` | yes |
| herdr GPUI, the desktop app, through its core `herdr-core` | `hosts.hc` | `hc:` and the terminal, such as `hc:t7` | `$HC_TERMINAL_ID` | no |
| [Orca](#orca) | `hosts.orca` | `orca:` and the worktree ID | `$ORCA_WORKTREE_ID`, or `$ORCA_AGENT_SESSION_ID` in a chat | no |
| [Rex](#rex) | `hosts.rex` | `rex:` and the block ID | `$REX_BLOCK` | no |
| [External host](#external-hosts) | `hosts.<name>` | `<name>:` and the host's workspace ID | `<command> caller` | yes, when listed |

`kelpie watch`, `kelpie unwatch` and `kelpie remind` act for the workspace of the terminal they run in. If several caller variables are set, `$TERM_PROGRAM` decides. A herdr pane ID is not the ID of a herdr workspace.

- [herdr](#herdr)
- [herdr GPUI](#herdr-gpui)
- [Orca](#orca)
- [Rex](#rex)
- [External hosts](#external-hosts)

## herdr

kelpie delivers each update with `herdr agent prompt`, and waits until the agent starts working on it. If the agent is blocked, for example at a question, the update waits until the agent can take input. If it waits longer than `delivery.notify.stuck` (5 minutes), herdr shows you a notification. If the agent does not react to an update, herdr shows you a notification too. A watch ends when its pane closes.

### herdr plugin

The plugin in [`herdr-plugin/`](../herdr-plugin) starts the daemon when herdr starts and lets you start watches without asking the agent. Link it from the checkout:

```sh
herdr plugin link "$PWD/herdr-plugin"
```

herdr runs the startup hook at the next herdr server start, not when you link the plugin. The plugin needs herdr 0.9.5 or later, and `kelpie`, `gh`, and `jq` on herdr's `PATH`. Set `KELPIE_BIN` to use a `kelpie` binary from somewhere else. The plugin adds:

- A startup hook. If no daemon answers, it runs `kelpie daemon` in the tab "daemon" of the workspace "kelpie". It creates the workspace and the tab if they do not exist.
- A link handler. Ctrl-click a pull request, issue, discussion, workflow run or Linear issue link in a pane to watch it for the pane's agent. A Linear link needs the `linear` provider registered. Ctrl is the modifier on macOS too. herdr handles the click only while `ui.mouse_capture` is on, which is the default.
- The action `kelpie.watch-pr`, which watches the PR of the focused pane's branch.
- The action `kelpie.status`, which opens `kelpie status` in a popup.

The two watch actions report through a herdr notification, such as `watching github:...` or `Not watching: <error>`. To bind the actions to keys, add this to `~/.config/herdr/config.toml`:

```toml
[[keys.command]]
key = "prefix+alt+w"
type = "plugin_action"
command = "kelpie.watch-pr"

[[keys.command]]
key = "prefix+alt+k"
type = "plugin_action"
command = "kelpie.status"
```

### Sidebar badges

kelpie reports a `kelpie` token on every pane it watches for. The token has one label per watched subject, separated by ` · `, for example `#2013 ✓ conflict · prometheus-community/pyrra 🔔22 · ⏰3`. kelpie also reports the token on the pane's workspace, with the subject labels of all its panes and without `⏰`.

| Label | Meaning |
| --- | --- |
| `#2013 ⋯` | Required checks are running. |
| `#2013 ✓` | Required checks passed. |
| `#2013 ✗` | A required check failed. |
| `draft`, `approved`, `changes requested` | The PR is a draft, or its review decision. |
| `conflict` | The PR conflicts with its base branch. |
| `issue #17 → #20` | A watched issue, and the open or merged PRs that will close it. `closed` follows the number of a closed issue. |
| `discussion #31 answered` | A watched discussion. `answered` and `closed` follow its number. |
| `run CI ⋯` | A watched GitHub Actions run and its workflow. `✓`, `✗` or `cancelled` replaces `⋯` when it ends. |
| `owner/repo 🔔22`, `org 🔔22` | Unread notification threads, up to the 50 most recent. |
| `owner/repo 🏷 v1.3.1` | The newest full release of a watched repo. |
| `ENG-123 In Review ⛔1` | A watched Linear issue, its status, and how many issues that block it are still open. |
| `⏰3` | Pending reminders for the pane. |

herdr shows the token only where your sidebar layout asks for it. Add a `$kelpie` row to `~/.config/herdr/config.toml`. The first two rows are herdr's default agent layout:

```toml
[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", "workspace", "tab"],
  ["agent"],
  [{ token = "$kelpie", dim = true }],
]
```

The same row works under `[ui.sidebar.spaces]`. The [herdr configuration docs](https://herdr.dev/tui/docs/configuration/) list the other sidebar options.

## herdr GPUI

The herdr desktop app runs its terminals in `herdr-core`. kelpie talks to that core over its unix socket. To turn the host on, set this in your [config file](configuration.md) and restart the daemon:

```yaml
hosts:
  hc:
    enabled: true
    socket: ""   # default: $HC_SOCKET, else the core's default socket
```

kelpie never starts the core. While the core is not running, updates wait. Each terminal is a workspace, and `kelpie status` lists it as `hc:` and the terminal, such as `hc:t7`. The core types an update into the terminal only when nobody has typed for a few seconds, and refuses it while the agent is blocked.

What differs from herdr:

- The core has no notifications. kelpie shows them through another enabled host, or not at all.
- The badge is the terminal label `kelpie`. The app does not draw it by itself; a mod can read it.
- kelpie cannot tell whether an agent runs in a terminal, so a branch shared with a GPUI terminal stays ambiguous for branch bindings.
- The herdr plugin is for the terminal UI only.

## Orca

kelpie can also deliver to agents in the Orca app, next to herdr or instead of it. The Orca host is off by default. To turn it on, set `hosts.orca.enabled: true` in your [config file](configuration.md) and restart the daemon. To use Orca without herdr, also set `hosts.herdr.enabled: false`.

```yaml
hosts:
  orca:
    enabled: true
    bin: ""         # default: $ORCA_CLI_COMMAND, else orca (orca-ide on Linux)
    poll: 15s
    comment: false  # true replaces the worktree's comment while a watch exists
```

A watch belongs to an Orca worktree, not to one terminal or chat. Run `kelpie watch` in an Orca terminal or chat and the watch goes to that worktree. A chat has no `$ORCA_WORKTREE_ID`, so kelpie looks up the worktree of the chat's `$ORCA_AGENT_SESSION_ID` with `orca worktree ps`. `kelpie status` calls the worktree a pane and lists it as `orca:` followed by the worktree ID.

Orca sends no events, so kelpie polls it every `hosts.orca.poll`. Updates go to the agent only while the agent is idle. An agent in a terminal gets them with `orca terminal send`. An agent in a chat gets them with `orca orchestration send`, which posts each update into the chat as a message from `kelpie`. If a worktree has several agents, the one Orca reports a state for and that was active last gets the update. kelpie never starts Orca. While Orca is not running, the updates wait.

Orca has no notification with a title and body. When updates wait longer than `delivery.notify.stuck`, kelpie sets the worktree's unread dot instead. A watch ends when its worktree is removed or archived. The herdr plugin and the sidebar badges are herdr only. With `comment: true`, the badge text goes into the worktree's comment, and kelpie puts your comment back when the worktree has no watch left. [DESIGN.md](../DESIGN.md) has the details in its section "Hosts: herdr and Orca".

## Rex

kelpie can deliver to agents in [Rex](https://www.superlogical.com/rex/). The Rex host is off by default. To turn it on, set this in your [config file](configuration.md) and restart the daemon:

```yaml
hosts:
  rex:
    enabled: true
    socket: ""   # default: $REX_SERVER if it is unix://, else Rex's default socket
    poll: 15s
```

kelpie talks to the Rex server on this machine over its socket. It does not reach a Rex server on another host. Each Rex pane is a workspace, and `kelpie status` lists it as `rex:` and the block ID, such as `rex:block:01a1ff5d-…`. Run `kelpie watch` in a Rex pane and the watch goes to that pane. A watch ends when its pane closes. kelpie never starts a Rex server. While none runs, the updates wait.

kelpie knows an agent by the status it reports to Rex. Claude Code does that by itself. kelpie sends an update only to a pane whose agent reports a status and is not waiting at a question, because Rex would type the update into whatever runs in the pane, a shell included. An agent that reports no status gets no updates in Rex.

What differs from herdr:

- Rex has no notifications that kelpie can send. kelpie shows them through another enabled host, or not at all.
- Rex has no sidebar badges. The pane label stays yours.
- There is no plugin. To start the daemon at login, use `kelpie service install`.

## External hosts

Like [providers](configuration.md#providers), a host integration can live in its own binary and language, talking to kelpie only over argv, stdin and stdout. Register it under `hosts:` in your [config file](configuration.md); kelpie runs only what the file names and never searches `PATH` for host binaries.

```yaml
hosts:
  myhost:
    command: [kelpie-host-myhost]
    poll: 15s      # optional; default 15s
    enabled: true  # optional; default true when listed
```

Host names must match `^[a-z][a-z0-9_-]*$` and cannot shadow built-in hosts (`herdr`, `orca`, `hc`, `rex`) or herdr workspace IDs (`^w[0-9]+$`). Workspace IDs are prefixed with `<name>:` inside kelpie (`myhost:s1`); the binary always receives and prints raw IDs without the prefix. Removing an external host from the config entirely makes kelpie treat its saved IDs as herdr panes, so the next herdr resync deletes those watches.

kelpie appends one subcommand to `command` on each call:

### `describe`

Called at startup and by `kelpie config show` (10s timeout). Prints YAML or JSON:

```yaml
kelpie: 1
name: myhost              # required; must match the key under hosts:
reports_agent: false      # optional; true if workspaces[].agent distinguishes agents from shells
term_program: ""          # optional; $TERM_PROGRAM value that wins nested caller detection
caller_env: [MYHOST_ID]   # optional; skip calling `caller` unless at least one listed env var is set
capabilities: []          # optional; subset of [badge, notify]
```

If `describe` fails, times out, or speaks a schema newer than `kelpie: 1`, kelpie skips that host with a warning, keeps its saved watches awaited so incoming events stay queued instead of dropping them, and continues with the rest.

### `caller`

Called by `kelpie watch`, `kelpie unwatch`, `kelpie remind` and `kelpie reminders` when `--workspace` is omitted, in the caller's environment and working directory. Prints the current workspace's raw ID on a single line and exits 0 if this shell belongs to the host; prints nothing or exits non-zero otherwise.

### `workspaces`

Called at startup, on a `kelpie watch` miss, and every `poll`. Prints a JSON array of live workspaces:

```json
[
  {
    "id": "s1",
    "path": "/path/to/repo",
    "branch": "fix-auth",
    "repos": ["owner/repo"],
    "agent": true,
    "state": "idle"
  }
]
```

Only `id` is required. When `path` is set and `branch` or `repos` is omitted, kelpie fills them from `git -C <path>` (cached for 30s). Hosts without a local checkout (such as web or chat agents) can leave `path`, `branch` and `repos` empty; explicit `kelpie watch` bindings still work. When `state` (`idle`, `done`, `working`, `blocked`, `unknown`) transitions to `idle` or `done`, kelpie releases queued deliveries for that workspace. On non-zero exit, kelpie keeps existing watches until the host answers again.

### `state <id>`

Prints one line on stdout: `idle`, `done`, `working`, `blocked` or `unknown`.

### `deliver <id>`

Reads the rendered prompt from stdin. Exit 0 confirms delivery. On non-zero exit, kelpie checks stderr (then stdout) for a JSON error object `{"code": "...", "message": "..."}`:

- `blocked` or `busy`: keeps the batch queued and retries later, notifying if stuck past `delivery.notify.stuck`.
- `unconfirmed`: does not retry (to avoid duplicate input) and notifies the user instead.
- Any other code or plain stderr: treated as a retryable delivery failure.

### `badge <id>` and `notify` (optional)

Called only when listed in `describe`'s `capabilities`. Both read JSON from stdin:

- `badge <id>` receives `{"text": "#421 ✓ · ⏰1", "subjects": ["#421 ✓"], "reminders": 1}` (empty text and subjects when clearing).
- `notify` receives `{"title": "...", "body": "...", "workspace": "s1", "workspace_host": "myhost", "needs_you": false}`. For a notification about another host's workspace, `workspace` keeps that host's prefix. Returning `{"code": "unsupported"}` with a non-zero exit lets another enabled host show the notification instead.
