#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd "${script_dir}/.." && pwd)"

if docker compose version >/dev/null 2>&1; then
  compose=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  compose=(docker-compose)
else
  echo "docker compose is required to validate deployment configuration" >&2
  exit 1
fi

while IFS='=' read -r key _; do
  if [[ "${key}" == MODEL_TRACING_* ]]; then
    unset "${key}"
  fi
done < "${deploy_dir}/.env.example"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
cat > "${tmp_dir}/required.env" <<'ENV'
POSTGRES_PASSWORD=compose-test
DATABASE_HOST=postgres-test
DATABASE_PASSWORD=compose-test
REDIS_HOST=redis-test
ENV
cp "${tmp_dir}/required.env" "${tmp_dir}/overrides.env"
while IFS='=' read -r key _; do
  if [[ "${key}" == MODEL_TRACING_* ]]; then
    printf '%s=%s\n' "${key}" "override-${key}" >> "${tmp_dir}/overrides.env"
  fi
done < "${deploy_dir}/.env.example"

for compose_file in docker-compose.yml docker-compose.standalone.yml; do
  for variant in defaults overrides; do
    env_file="${tmp_dir}/required.env"
    if [[ "${variant}" == overrides ]]; then
      env_file="${tmp_dir}/overrides.env"
    fi
    "${compose[@]}" \
      --env-file "${env_file}" \
      -f "${deploy_dir}/${compose_file}" \
      config --format json > "${tmp_dir}/${compose_file}.${variant}.json"
  done
done

python3 - \
  "${deploy_dir}/.env.example" \
  "${tmp_dir}/overrides.env" \
  "${tmp_dir}/docker-compose.yml.defaults.json" \
  "${tmp_dir}/docker-compose.standalone.yml.defaults.json" \
  "${tmp_dir}/docker-compose.yml.overrides.json" \
  "${tmp_dir}/docker-compose.standalone.yml.overrides.json" <<'PY'
import json
import pathlib
import sys

def model_tracing_values(path):
    values = {}
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        if key.startswith("MODEL_TRACING_"):
            values[key] = value
    return values

env_path = pathlib.Path(sys.argv[1])
defaults = model_tracing_values(env_path)
overrides = model_tracing_values(pathlib.Path(sys.argv[2]))
if not defaults:
    raise SystemExit(f"no MODEL_TRACING_* defaults found in {env_path}")
if defaults.keys() != overrides.keys():
    raise SystemExit("override fixture does not cover every MODEL_TRACING_* default")

failures = []
for rendered_path in map(pathlib.Path, sys.argv[3:]):
    expected = overrides if ".overrides." in rendered_path.name else defaults
    rendered = json.loads(rendered_path.read_text(encoding="utf-8"))
    actual = rendered["services"]["sub2api"].get("environment", {})
    for key, value in expected.items():
        if key not in actual:
            failures.append(f"{rendered_path.name}: missing {key}")
        elif str(actual[key]) != value:
            failures.append(
                f"{rendered_path.name}: {key}={actual[key]!r}, expected {value!r}"
            )

if failures:
    raise SystemExit("\n".join(failures))

print(
    f"validated defaults and overrides for {len(defaults)} MODEL_TRACING_* variables in "
    f"{len(sys.argv) - 3} rendered Compose files"
)
PY
