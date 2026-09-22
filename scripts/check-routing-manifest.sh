#!/usr/bin/env bash
# Check the documented pane route manifest against real tests and the default E2E.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UI="$ROOT/docs/ui-routing-verification.md"
PANES="$ROOT/docs/pane-routing.md"
E2E="$ROOT/scripts/ducklord-tui-e2e.sh"
TESTDIR="$ROOT/cmd/ducklord"

mapfile -t ui_rows < <(awk -F'|' '/^\| `route\./ { print $2 "\t" $3 "\t" $4 }' "$UI" | sed 's/^ *//; s/ *\t/\t/; s/ *$//')
mapfile -t pane_routes < <(awk -F'|' '/^\| `route\./ { print $2 }' "$PANES" | sed 's/^ *//; s/ *$//' | sort -u)

if ((${#ui_rows[@]} == 0)); then
	printf 'routing manifest has no route rows: %s\n' "$UI" >&2
	exit 1
fi

failures=0
seen_routes=()
for row in "${ui_rows[@]}"; do
	IFS=$'\t' read -r route state e2e <<<"$row"
	seen_routes+=("$route")
	if [[ "$state" =~ (none|state-only|opt-in|targeted) || "$e2e" =~ (none|state-only|opt-in|targeted) ]]; then
		printf '%s: state and default E2E evidence are required\n' "$route" >&2
		failures=$((failures + 1))
	fi
	if [[ "$e2e" != *TestDucklord*E2E* ]]; then
		printf '%s: missing TestDucklord*E2E mapping\n' "$route" >&2
		failures=$((failures + 1))
	fi
	while read -r test_name; do
		[[ -z "$test_name" ]] && continue
		if ! rg -q "^[[:space:]]*func ${test_name}\(" "$TESTDIR"; then
			printf '%s: missing test function %s\n' "$route" "$test_name" >&2
			failures=$((failures + 1))
		fi
		if [[ "$test_name" == TestDucklord*E2E ]]; then
			# The default script may list a shared suffix inside a grouped regex
			# rather than the complete Go test function name.
			e2e_token="${test_name#TestDucklord}"
			e2e_token="${e2e_token%E2E}"
			if ! rg -q "$test_name|$e2e_token" "$E2E"; then
				printf '%s: %s is absent from the default E2E script\n' "$route" "$test_name" >&2
				failures=$((failures + 1))
			fi
		fi
	done < <(printf '%s\n%s\n' "$state" "$e2e" | rg -o 'Test[A-Za-z0-9_]+' | sort -u)
done

for route in "${seen_routes[@]}"; do
	if ! printf '%s\n' "${pane_routes[@]}" | rg -qxF "$route"; then
		printf '%s: missing from pane-routing.md\n' "$route" >&2
		failures=$((failures + 1))
	fi
done
if ((${#seen_routes[@]} != ${#pane_routes[@]})); then
	printf 'route row count differs between routing documents\n' >&2
	failures=$((failures + 1))
fi

if ((failures)); then
	exit 1
fi
printf 'routing manifest PASS: %d routes, state tests, and default E2E mappings verified\n' "${#seen_routes[@]}"
