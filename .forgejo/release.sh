#!/usr/bin/env bash
# Forgejo release helper for the .forgejo/workflows (gh only talks to GitHub).
#   .forgejo/release.sh <tag> <title> <notes> [--draft] [--replace] [--target <sha>] -- <files...>
# Creates the release <tag> of this repo (or reuses it), uploads the files (an asset with the same name is
# replaced). --replace deletes an existing release + tag first (test builds, the Play bundle).
# Needs FORGEJO_TOKEN (the job's token), GITHUB_SERVER_URL, GITHUB_REPOSITORY, GITHUB_SHA; curl and jq.
set -euo pipefail
tag=$1 title=$2 notes=$3; shift 3
draft=false replace=false target=${GITHUB_SHA}
while [ $# -gt 0 ]; do
  case $1 in
    --draft) draft=true; shift ;;
    --replace) replace=true; shift ;;
    --target) target=$2; shift 2 ;;
    --) shift; break ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
done
api="${GITHUB_SERVER_URL%/}/api/v1/repos/${GITHUB_REPOSITORY}"
auth=(-H "Authorization: token ${FORGEJO_TOKEN:?FORGEJO_TOKEN not set}")

# the release with this tag, drafts included (/releases/tags/<tag> skips drafts)
find_id() { curl -fsS "${auth[@]}" "$api/releases?limit=50&draft=true" | jq -r --arg t "$tag" '[.[] | select(.tag_name == $t)][0].id // empty'; }

id=$(find_id)
if [ -n "$id" ] && $replace; then
  curl -fsS -X DELETE "${auth[@]}" "$api/releases/$id" >/dev/null
  curl -sS -X DELETE "${auth[@]}" "$api/tags/$tag" >/dev/null || true
  id=""
fi
if [ -z "$id" ]; then
  body=$(jq -n --arg t "$tag" --arg n "$title" --arg b "$notes" --arg c "$target" --argjson d "$draft" \
    '{tag_name: $t, name: $n, body: $b, target_commitish: $c, draft: $d, prerelease: false}')
  id=$(curl -fsS -X POST "${auth[@]}" -H "Content-Type: application/json" -d "$body" "$api/releases" | jq -r .id)
  echo "release $tag created (id $id, draft=$draft)"
else
  echo "release $tag exists (id $id)"
fi

for f in "$@"; do
  name=$(basename "$f")
  old=$(curl -fsS "${auth[@]}" "$api/releases/$id/assets" | jq -r --arg n "$name" '.[] | select(.name == $n) | .id')
  for a in $old; do curl -fsS -X DELETE "${auth[@]}" "$api/releases/$id/assets/$a" >/dev/null; done
  curl -fsS -X POST "${auth[@]}" -F "attachment=@$f" "$api/releases/$id/assets?name=$(jq -rn --arg n "$name" '$n|@uri')" >/dev/null
  echo "  + $name ($(du -h "$f" | cut -f1))"
done
echo "${GITHUB_SERVER_URL%/}/${GITHUB_REPOSITORY}/releases/tag/$tag"
