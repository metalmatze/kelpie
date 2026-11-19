# Configuration

kelpie runs on the built-in default, [`internal/config/default.yaml`](../internal/config/default.yaml), and the sources of the [providers](#providers) your file registers. Your file, `~/.config/kelpie/config.yaml` (or under `$XDG_CONFIG_HOME`), layers over both. Put only what you change in your file. Everything else follows the kelpie and provider binaries you run, so new sources and fixes reach you without copying. To write a commented starter file and print the config in effect:

```sh
kelpie init
kelpie config show
```

For example, this file registers GitHub, turns on Orca, keeps herdr and every GitHub source but the org notifications source, and adds a jq helper:

```yaml
providers:
  github: {command: [kelpie-github]}
hosts:
  orca:
    enabled: true
sources:
  - {name: github-inbox-org, enabled: false}
defs: |
  def short: .[:7];
```

The merge rules are:

- Settings merge key by key.
- A list, such as `rules.ignore_actors` or `delivery.notify.kinds`, replaces the default's list.
- A source with the name of a provider's source replaces that source completely, not field by field. A source with a new name is added.
- An entry with only `name` and `enabled: false` switches off a provider's source.
- `defs` are added after the providers' jq definitions.

When your file replaces a provider's source or redefines a provider's def, kelpie prints a warning at start and in `kelpie config show`. Delete those entries from your file unless you changed them on purpose. A file that `kelpie init` wrote before providers existed is a full copy of the default at that time, GitHub's sources included. It gets these warnings for every source. Trim it to the settings you changed, or move it away and run `kelpie init` again.

Unknown keys are errors. kelpie reads the file only at start, so restart the daemon after you edit it. Every command takes `--config <path>` to use another file.

Settings you may want to change:

| Setting | Default | What it does |
| --- | --- | --- |
| `delivery.debounce` | `30s` | How long events wait, so a burst arrives as one update. |
| `delivery.urgent_debounce` | `5s` | The same for failed checks, merges and closed issues. |
| `delivery.notify.stuck` | `5m` | When to tell you that updates could not be delivered. |
| `rules.allow_bots` | Cursor, CodeRabbit, Copilot reviewer | Bots whose actions are reported; other bots are ignored. |
| `reminders.catch_up` | `12h` | How late a missed reminder may still fire. |

The comments in the default file describe the rest.

## Sources

The config defines each source as a command (`gh` for GitHub, `curl` for Linear), a jq mapping, and the transitions that become events. A new source needs no Go code. The `github` provider has `github-pr`, `github-checks`, `github-issue`, `github-discussion`, `github-run`, `github-inbox`, `github-inbox-org`, and `github-release`; the `linear` provider has `linear` and `linear-issue`. To run one source once and print what it observes, use `kelpie source test`. `github-pr`, `github-issue`, `github-discussion`, `github-run`, `github-release`, `linear-issue`, and the inbox sources need `--subject`:

```sh
kelpie source test github-pr --subject https://github.com/prometheus-community/pyrra/pull/2013
```

## Providers

A provider is an integration in a binary of its own: `kelpie-github` and `kelpie-linear`. kelpie runs only the providers your file lists, with the command you give. It never searches `PATH` for other `kelpie-*` binaries:

```yaml
providers:
  github: {command: [kelpie-github]}
  linear: {command: [/Users/me/go/bin/kelpie-linear]}
```

At start, kelpie runs `<command> describe`. The provider prints its sources, its credential, the branch names that bind its subjects, and the URLs and IDs it parses. `kelpie-github` also names a `me` command, which prints the login `gh` acts as. The daemon runs it at start, so `@me` in `rules.ignore_actors` drops your own GitHub activity. Its sources layer between the default and your file, so a source of the same name in your file replaces the provider's. `kelpie config show` marks each source that came from a provider and lists what each provider contributed. A provider whose `describe` fails or takes longer than 10 seconds is skipped with a warning, and kelpie runs without it.

`kelpie watch <provider> <target>` runs `<command> subject <target>` in your shell, in the current directory, and watches what it prints. `kelpie watch <target>` without a provider name works for a URL or ID that exactly one provider claims, such as a Linear issue URL. If two providers claim it, kelpie asks for the provider name. A subject, as `kelpie status` lists it, needs no provider. kelpie refuses anything else and lists the providers. An org name alone needs its provider, as in `kelpie watch github org`, because any provider's ID could be a bare word.

To stop a provider from binding its issues to panes by branch, set `binders.<provider>: false`, such as `binders.linear: false`. To remove a provider, delete its entry under `providers:`.
