#!/usr/bin/env bash
# install.sh — build `convo` and symlink the agent-conversations skill into place.
#
# Usage:
#   ./install.sh              install (build + symlinks)
#   ./install.sh --force      install, replacing existing symlinks
#   ./install.sh --uninstall  remove only the symlinks this script would create
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd -P)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." >/dev/null 2>&1 && pwd -P)"

SKILL_NAME="agent-conversations"
SKILL_SRC="$REPO_ROOT/skill"
BIN_SRC="$REPO_ROOT/bin/convo"

CLAUDE_SKILLS_DIR="$HOME/.claude/skills"
AGENTS_SKILLS_DIR="$HOME/.agents/skills"
LOCAL_BIN_DIR="$HOME/.local/bin"

FORCE=0
UNINSTALL=0
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    --uninstall) UNINSTALL=1 ;;
    -h|--help)
      sed -n '2,7p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *)
      echo "unknown flag: $arg" >&2
      exit 1
      ;;
  esac
done

link() {
  # link <target> <link_path>
  local target="$1" link_path="$2"
  mkdir -p "$(dirname -- "$link_path")"

  if [ -L "$link_path" ]; then
    local existing
    existing="$(readlink "$link_path")"
    if [ "$existing" = "$target" ]; then
      echo "already linked: $link_path -> $target"
      return 0
    fi
    if [ "$FORCE" -eq 1 ]; then
      rm -f "$link_path"
    else
      echo "warning: $link_path already symlinks elsewhere ($existing) — use --force to replace" >&2
      return 0
    fi
  elif [ -e "$link_path" ]; then
    echo "warning: $link_path exists and is not a symlink — refusing to clobber" >&2
    return 0
  fi

  ln -s "$target" "$link_path"
  echo "linked: $link_path -> $target"
}

unlink_if_ours() {
  # unlink_if_ours <target> <link_path>
  local target="$1" link_path="$2"
  if [ -L "$link_path" ]; then
    local existing
    existing="$(readlink "$link_path")"
    if [ "$existing" = "$target" ]; then
      rm -f "$link_path"
      echo "unlinked: $link_path"
      return 0
    fi
    echo "skipped (not ours): $link_path -> $existing" >&2
    return 0
  fi
  if [ -e "$link_path" ]; then
    echo "skipped (not a symlink): $link_path" >&2
    return 0
  fi
  echo "not present: $link_path"
}

if [ "$UNINSTALL" -eq 1 ]; then
  unlink_if_ours "$SKILL_SRC" "$CLAUDE_SKILLS_DIR/$SKILL_NAME"
  unlink_if_ours "$SKILL_SRC" "$AGENTS_SKILLS_DIR/$SKILL_NAME"
  unlink_if_ours "$BIN_SRC" "$LOCAL_BIN_DIR/convo"
  echo ""
  echo "uninstalled. The repo itself was not touched."
  exit 0
fi

echo "building the CLI…"
make -C "$REPO_ROOT" build

link "$SKILL_SRC" "$CLAUDE_SKILLS_DIR/$SKILL_NAME"
link "$SKILL_SRC" "$AGENTS_SKILLS_DIR/$SKILL_NAME"
link "$BIN_SRC" "$LOCAL_BIN_DIR/convo"

echo ""
case ":$PATH:" in
  *":$LOCAL_BIN_DIR:"*)
    echo "PATH:  $LOCAL_BIN_DIR is on PATH"
    ;;
  *)
    echo "PATH:  warning — $LOCAL_BIN_DIR is NOT on PATH; add it (e.g. export PATH=\"\$HOME/.local/bin:\$PATH\")" >&2
    ;;
esac

# Herdr is a hard prerequisite, not an optional integration: delivery into a
# running agent session always goes through it. Say so plainly at install time
# rather than letting the first `host deliver` fail with exit 69.
if command -v herdr >/dev/null 2>&1; then
  echo "herdr: found at $(command -v herdr)"
else
  echo "herdr: NOT FOUND — required. Without it, delivery into an agent session" >&2
  echo "       cannot work at all (host commands will exit 69)." >&2
fi

echo ""
echo "Runtimes that read AGENTS.md rather than a skill dir: $SKILL_SRC/AGENTS.md is self-contained."
echo ""
echo "now run: convo self"
