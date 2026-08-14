#!/bin/bash
set -euo pipefail

bin=${1:-./bin/loopctl}
bin=$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
repo="$tmp/repo"
state="$tmp/state"
git init -q "$repo"
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name Test
touch "$repo/README.md"
git -C "$repo" add README.md
git -C "$repo" commit -qm init
git -C "$repo" branch dev
git -C "$repo" remote add origin https://github.com/test/repo.git
# macOS exposes /var through /private/var. Match loopctl's canonical repo_path.
repo=$(cd "$repo" && pwd -P)
"$bin" init --offline --project stress --repo-path "$repo" --state-root "$state" >/dev/null

script_dir=$(cd "$(dirname "$0")" && pwd)
card() {
  id=$1
  touch=$2
  sed -e "s/__ID__/$id/g" -e "s#__TOUCH__#$touch#g" -e "s#__REPO_PATH__#$repo#g" -e 's#__REPO__#test/repo#g' "$script_dir/fixtures/card.json"
}

card t1 src/a.go | "$bin" add --stdin --state-root "$state" >/dev/null
success=0
pids=()
for i in $(seq 1 20); do
  ("$bin" claim --role dev --worker "w$i" --state-root "$state" >"$tmp/t1-$i" 2>/dev/null) & pids+=("$!")
done
for pid in "${pids[@]}"; do if wait "$pid"; then success=$((success+1)); fi; done
[ "$success" -eq 1 ] || { echo "T1 failed: $success winners"; exit 1; }
"$bin" doctor --state-root "$state" >/dev/null
"$bin" config set limits.max_in_flight 10 --state-root "$state" >/dev/null

for id in t2a t2b t2c t2d t2e; do card "$id" "src/$id.go" | "$bin" add --stdin --state-root "$state" >/dev/null; done
success=0
pids=()
for i in $(seq 1 20); do
  ("$bin" claim --role dev --worker "x$i" --state-root "$state" >"$tmp/t2-$i" 2>/dev/null) & pids+=("$!")
done
for pid in "${pids[@]}"; do if wait "$pid"; then success=$((success+1)); fi; done
[ "$success" -eq 5 ] || { echo "T2 failed: $success winners"; exit 1; }
"$bin" doctor --state-root "$state" >/dev/null

"$bin" config set limits.max_in_flight 200 --state-root "$state" >/dev/null
for i in $(seq 1 10); do
  card "qa$i" "qa/$i.go" | "$bin" add --stdin --state-root "$state" >/dev/null
  "$bin" claim --role dev --worker "prep$i" --state-root "$state" >/dev/null
  "$bin" move "qa$i" --to in_review --by "dev/prep$i" --patch "{\"pr\":$i}" --state-root "$state" >/dev/null
done
for i in $(seq 1 10); do
  card "dev$i" "dev/$i.go" | "$bin" add --stdin --state-root "$state" >/dev/null
done
pids=()
for i in $(seq 1 25); do
  ("$bin" claim --role dev --worker "mixd$i" --state-root "$state" >/dev/null 2>&1 || true) & pids+=("$!")
  ("$bin" claim --role qa --worker "mixq$i" --state-root "$state" >/dev/null 2>&1 || true) & pids+=("$!")
done
for pid in "${pids[@]}"; do wait "$pid"; done
"$bin" doctor --state-root "$state" >/dev/null

pids=()
for i in $(seq 1 50); do
  card "sync$i" "sync/$i.go" >"$tmp/sync$i.json"
  ("$bin" add --file "$tmp/sync$i.json" --state-root "$state" >/dev/null 2>&1 || true) & pids+=("$!")
  ("$bin" claim --role dev --worker "sync$i" --state-root "$state" >/dev/null 2>&1 || true) & pids+=("$!")
done
for pid in "${pids[@]}"; do wait "$pid"; done
"$bin" doctor --state-root "$state" >/dev/null

for phase in prepared destination-created source-removed; do
  card "crash-$phase" "crash/$phase.go" | "$bin" add --stdin --state-root "$state" >/dev/null
  set +e
  LOOPCTL_FAULT_PHASE="$phase" "$bin" claim --role dev --worker "crash-$phase" --state-root "$state" >/dev/null 2>&1
  set -e
  "$bin" doctor --state-root "$state" >/dev/null
done

echo "stress: T1-T5 passed"
