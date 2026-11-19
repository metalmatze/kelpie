# The daemon

`kelpie daemon` does the work: it polls what you watch, turns changes into events, and delivers them. Only one daemon runs at a time. It holds a lock, so a second `kelpie daemon` prints that one is already running and exits.

To try kelpie without typing into any terminal, run `kelpie daemon --dry-run`. It prints each update instead. `--debug` also logs the workspaces it sees and every poll.

## Polling

The daemon runs `gh` on a timer for every watched subject. By default, it polls:

| Subject | Every |
| --- | --- |
| A PR | 30 seconds |
| A PR's checks, while they run | 15 seconds |
| A GitHub Actions run | 10 seconds |
| An issue, a discussion, a notification inbox | 60 seconds |
| A repo's releases | hour |
| A watched Linear issue | 60 seconds |
| The Linear issues assigned to you | 2 minutes |

For Linear, which has no CLI, the sources that `kelpie-linear` describes run `curl` against Linear's GraphQL API with the key from `kelpie auth login linear`. The daemon runs these itself; it calls `kelpie-linear` only at start.

kelpie compares each result with the previous one and turns the differences into [events](watches.md#events). By default, events wait 30 seconds so that a burst arrives as one update. Failed checks, merges, and closed issues wait 5 seconds. Each pane gets its own batch, so two panes that watch the same PR both get its updates.

## State

kelpie saves watches and reminders in `~/.local/state/kelpie/`, or under `$XDG_STATE_HOME` if you set it. With `hosts.orca.comment`, the same directory holds the Orca comments that kelpie replaced. Everything else lives in memory, so after a restart kelpie takes a fresh baseline and does not report what changed while it was down.

## Run the daemon at login

With herdr, skip this section: the [herdr plugin](hosts.md#herdr-plugin) starts the daemon each time herdr starts. Install a unit only if you run agents without herdr, for example in Orca alone, or if kelpie must run while herdr is closed.

`kelpie service install` runs `kelpie daemon` as a launchd LaunchAgent on macOS or as a systemd user unit on Linux, and starts it:

```sh
kelpie service install
```

The unit runs the `kelpie` binary you ran the command with. It also gets your `--config` path, your shell's `PATH` without its temporary directories, and `XDG_STATE_HOME` and `XDG_CONFIG_HOME` if they are set. Run `kelpie service install` again after the binary path, `--config`, or one of those variables changes. If the daemon fails, launchd or systemd restarts it after 30 seconds. On Linux, the unit starts with your login session unless you run `loginctl enable-linger $USER`.

On macOS, the unit is `~/Library/LaunchAgents/dev.kelpie.daemon.plist` and the log is `~/Library/Logs/dev.kelpie.daemon.log`. On Linux, the unit is `~/.config/systemd/user/kelpie.service` and the log is in `journalctl --user -u kelpie.service`. `kelpie service status` shows whether the unit is installed and running, and `kelpie service uninstall` stops the daemon and removes the unit. To see the unit without installing it, run `kelpie service install --print`.

You can set up both the unit and the herdr plugin, and the first of them to start runs the daemon. [DESIGN.md](../DESIGN.md) explains the lock in the paragraph "Daemon lifecycle" under "Developer experience".
