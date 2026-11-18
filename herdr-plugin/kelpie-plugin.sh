#!/usr/bin/env bash
# Commands of the kelpie herdr plugin. Actions have no terminal, so they
# answer with a herdr notification.
set -uo pipefail

herdr="${HERDR_BIN_PATH:-herdr}"
kelpie="${KELPIE_BIN:-kelpie}"

notify() {
	echo "$1" # kept in herdr plugin log list
	"$herdr" notification show "kelpie" --body "$1" --sound none >/dev/null 2>&1 || true
}

# watch runs kelpie watch for the pane the action was invoked in and reports
# kelpie's answer, or its error.
watch() {
	local pane="${HERDR_PANE_ID:-}"
	if [[ -z "$pane" ]]; then
		notify "No pane to watch for: focus an agent's pane first."
		return
	fi
	# No provider name: kelpie gives the link to the provider whose target
	# patterns claim it, kelpie-github for a GitHub link.
	local out
	if out=$("$kelpie" watch "$1" --workspace "$pane" 2>&1); then
		notify "$(head -n1 <<<"$out")"
	else
		notify "Not watching: ${out#kelpie: error: }"
	fi
}

case "${1:-}" in
startup)
	"$kelpie" status >/dev/null 2>&1 && exit 0
	# In the tab "daemon" of the workspace "kelpie": a plugin pane would open
	# as a tab of whatever workspace has focus. herdr restores both with an
	# idle shell after a restart, so reuse them rather than add one per start.
	ws=$("$herdr" workspace list | jq -r '[.result.workspaces[] | select(.label == "kelpie")][0].workspace_id // empty')
	if [[ -z "$ws" ]]; then
		ws=$("$herdr" workspace create --label kelpie --cwd "$HOME" --no-focus | jq -r .result.workspace.workspace_id)
	fi
	tab=$("$herdr" tab list --workspace "$ws" | jq -r '[.result.tabs[] | select(.label == "daemon")][0].tab_id // empty')
	if [[ -z "$tab" ]]; then
		pane=$("$herdr" tab create --workspace "$ws" --label daemon --no-focus | jq -r .result.root_pane.pane_id)
	else
		pane=$("$herdr" pane list --workspace "$ws" | jq -r --arg tab "$tab" '[.result.panes[] | select(.tab_id == $tab)][0].pane_id')
	fi
	"$herdr" pane run "$pane" "$kelpie daemon" >/dev/null
	;;
watch-link)
	watch "${HERDR_PLUGIN_CLICKED_URL:?}"
	;;
watch-pr)
	pane="${HERDR_PANE_ID:-}"
	if [[ -z "$pane" ]]; then
		notify "No pane to watch for: focus an agent's pane first."
		exit 0
	fi
	dir=$("$herdr" pane get "$pane" | jq -r '.result.pane.foreground_cwd // .result.pane.cwd')
	# gh pr view finds the branch's PR the way gh does everywhere, upstream
	# and fork remotes included.
	if ! url=$(cd "$dir" && gh pr view --json url --jq .url 2>&1); then
		notify "No pull request for the branch in $dir: ${url}"
		exit 0
	fi
	watch "$url"
	;;
status)
	exec "$herdr" plugin pane open --plugin kelpie --entrypoint status
	;;
status-popup)
	"$kelpie" status
	printf '\npress any key to close'
	read -rsn1
	;;
*)
	echo "usage: kelpie-plugin.sh startup|watch-link|watch-pr|status|status-popup" >&2
	exit 2
	;;
esac
