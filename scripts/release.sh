#!/usr/bin/env bash
# release.sh — Create a kamori-go release
#
# Go modules are versioned by git tag. No files need to be bumped.
# Pushing the tag triggers release.yml which runs tests and creates a GitHub Release.
# pkg.go.dev indexes the module automatically.
#
# Usage:
#   ./scripts/release.sh <version> [--dry-run]
#
#   version   patch | minor | major | x.y.z | x.y.z-pre.n
#   --dry-run show plan without making changes
#
# Examples:
#   ./scripts/release.sh patch
#   ./scripts/release.sh minor
#   ./scripts/release.sh 1.2.0
#   ./scripts/release.sh 1.0.0-rc.1

set -euo pipefail

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }
die()    { red "error: $*" >&2; exit 1; }
step()   { echo ""; bold "$*"; }

usage() {
  grep '^#' "$0" | sed -n '/Usage:/,/^[^#]/p' | sed 's/^# \?//' | head -n -1
  exit 1
}

VERSION_ARG=""
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=true; shift ;;
    -h|--help) usage ;;
    -*)        die "unknown flag: $1" ;;
    *)
      [[ -n "$VERSION_ARG" ]] && die "unexpected argument: $1"
      VERSION_ARG="$1"; shift ;;
  esac
done

[[ -z "$VERSION_ARG" ]] && usage

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git repo"
cd "$REPO_ROOT"

# ─── pre-flight ───────────────────────────────────────────────────────────────
step "Pre-flight checks"

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ "$BRANCH" == "main" ]] || die "must be on main (currently on '$BRANCH')"
green "  ✓ on main"

[[ -z "$(git status --porcelain)" ]] || die "working tree has uncommitted changes — commit or stash first"
green "  ✓ clean working tree"

git fetch --quiet origin
BEHIND="$(git rev-list --count HEAD..origin/main 2>/dev/null || echo 0)"
[[ "$BEHIND" == "0" ]] || die "local main is $BEHIND commit(s) behind origin/main — run 'git pull' first"
green "  ✓ up to date with origin/main"

# ─── resolve new version ──────────────────────────────────────────────────────
LATEST_TAG="$(git tag -l 'v*' | sort -V | tail -1)"
CURRENT="${LATEST_TAG#v}"
[[ -z "$CURRENT" ]] && CURRENT="0.0.0"

case "$VERSION_ARG" in
  patch|minor|major)
    NEW_VERSION="$(node -e "
      const [ma, mi, pa] = '${CURRENT}'.split('-')[0].split('.').map(Number);
      const t = '${VERSION_ARG}';
      if (t === 'major') process.stdout.write((ma+1)+'.0.0');
      else if (t === 'minor') process.stdout.write(ma+'.'+(mi+1)+'.0');
      else process.stdout.write(ma+'.'+mi+'.'+(pa+1));
    " 2>/dev/null)" || die "node is required to compute semver bumps"
    ;;
  [0-9]*)
    NEW_VERSION="$VERSION_ARG"
    ;;
  *)
    die "invalid version '$VERSION_ARG'. Use patch|minor|major or explicit semver"
    ;;
esac

[[ "$NEW_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] || die "could not compute a valid semver from '$VERSION_ARG'"

TAG="v${NEW_VERSION}"

# ─── check tag doesn't already exist ─────────────────────────────────────────
git tag -l "$TAG" | grep -q "$TAG" && die "tag $TAG already exists locally"
git ls-remote --tags origin "refs/tags/$TAG" 2>/dev/null | grep -q "$TAG" && die "tag $TAG already exists on remote"

# ─── show plan ────────────────────────────────────────────────────────────────
step "Release plan"
echo "  Tag:     $TAG"
[[ -n "$LATEST_TAG" ]] && echo "  Prev:    $LATEST_TAG"
echo "  Trigger: release.yml → test → GitHub Release"
echo "  Module:  github.com/usekamori/kamori-go@$TAG"
echo "  No files change — Go modules are versioned by tag."
echo ""

if $DRY_RUN; then
  yellow "Dry-run — no changes made."
  exit 0
fi

read -r -p "Proceed? [y/N] " CONFIRM
[[ "$CONFIRM" =~ ^[Yy]$ ]] || { yellow "Aborted."; exit 0; }

# ─── tag + push ───────────────────────────────────────────────────────────────
step "Tagging $TAG"
git tag "$TAG"
git push origin "$TAG"
green "  ✓ tag pushed — release.yml is now running"

echo ""
bold "Done."
echo "  https://github.com/usekamori/kamori-go/actions"
echo ""
echo "  Users can now install with:"
echo "    go get github.com/usekamori/kamori-go@$TAG"
echo ""
