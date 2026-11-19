# Hosts: herdr, herdr GPUI and Orca

kelpie delivers updates through the program the agent runs in, the host. It supports three:

| Host | Config | Workspace ID | Caller variable | On by default |
| --- | --- | --- | --- | --- |
| [herdr](https://herdr.dev), the terminal UI | `hosts.herdr` | a pane, such as `w1:p2` | `$HERDR_PANE_ID` | yes |
| herdr GPUI, the desktop app, through its core `herdr-core` | `hosts.hc` | `hc:` and the terminal, such as `hc:t7` | `$HC_TERMINAL_ID` | no |
| [Orca](#orca) | `hosts.orca` | `orca:` and the worktree ID | `$ORCA_WORKTREE_ID`, or `$ORCA_AGENT_SESSION_ID` in a chat | no |

`kelpie watch`, `kelpie unwatch` and `kelpie remind` act for the workspace of the terminal they run in. If several caller variables are set, `$TERM_PROGRAM` decides. A herdr pane ID is not the ID of a herdr workspace.

- [herdr](#herdr)
- [herdr GPUI](#herdr-gpui)
- [Orca](#orca)
- [Planned: Rex](#planned-rex)

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

## Planned: Rex

Rex support is planned. Work starts as soon as kelpie's maintainer has access to Rex.
