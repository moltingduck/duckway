#!/usr/bin/env bash
# Default duckway agent statusline.
# Reads Claude Code's status payload (JSON) on stdin, prints one line
# with: working folder · 5-hour usage · 7-day usage.
#
# Admin can replace this from /admin/settings → Agent Statusline.
# Falls back to a hint when `jq` is missing so the agent doesn't see
# a silent failure.

input=$(cat)

if ! command -v jq >/dev/null 2>&1; then
  echo "[duckway statusline] install jq to enable usage display"
  exit 0
fi

# Helper: jq with safe stdin handling.
j() { printf '%s' "$input" | jq -r "$1 // empty" 2>/dev/null; }

cwd=$(j '.workspace.current_dir // .cwd')
folder=""
[ -n "$cwd" ] && folder=$(basename "$cwd")

five_pct=$(j '.rate_limits.five_hour.used_percentage')
five_reset=$(j '.rate_limits.five_hour.resets_at')
week_pct=$(j '.rate_limits.seven_day.used_percentage')
week_reset=$(j '.rate_limits.seven_day.resets_at')

# Cyan folder, magenta usage. Skip colors if NO_COLOR is set.
if [ -n "$NO_COLOR" ]; then
  C_FOLDER=""; C_USAGE=""; C_RESET=""
else
  C_FOLDER=$'\033[0;36m'
  C_USAGE=$'\033[0;35m'
  C_RESET=$'\033[0m'
fi

parts=()
[ -n "$folder" ] && parts+=("${C_FOLDER}${folder}${C_RESET}")

fmt_pct() {
  # Round to int, append "%". printf %.0f available in bash + dash.
  printf '%.0f%%' "$1" 2>/dev/null || printf '%s' "$1"
}

fmt_reset() {
  # Linux GNU date and BSD date have different -d / -r flags.
  date -d "@$1" "+%H:%M" 2>/dev/null || date -r "$1" "+%H:%M" 2>/dev/null
}

if [ -n "$five_pct" ]; then
  s="5h:${C_USAGE}$(fmt_pct "$five_pct")${C_RESET}"
  if [ -n "$five_reset" ]; then
    r=$(fmt_reset "$five_reset")
    [ -n "$r" ] && s="$s→$r"
  fi
  parts+=("$s")
fi
if [ -n "$week_pct" ]; then
  s="7d:${C_USAGE}$(fmt_pct "$week_pct")${C_RESET}"
  if [ -n "$week_reset" ]; then
    r=$(fmt_reset "$week_reset")
    [ -n "$r" ] && s="$s→$r"
  fi
  parts+=("$s")
fi

out=""
for p in "${parts[@]}"; do
  if [ -z "$out" ]; then out="$p"; else out="$out | $p"; fi
done
printf '%b\n' "$out"
