#!/usr/bin/env bash
# 真实 HTTP 协议矩阵：复用 run_e2e.sh 已启动的 sub2api、fixture 和 Langfuse。
set -euo pipefail

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
BASE_URL="${BASE_URL:-http://localhost:8080}"
TOKEN="${TOKEN:?TOKEN is required}"
RUN_ID="${RUN_ID:-$(date +%s)-$$}"
PREFIX="e2e-matrix-${RUN_ID}"
TMP_DIR="$REPO_ROOT/.e2e-tmp/protocol-matrix-$RUN_ID"
mkdir -p "$TMP_DIR"

log() { printf '[e2e-matrix] %s\n' "$*" >&2; }
fail() { printf '[e2e-matrix][ERROR] %s\n' "$*" >&2; exit 1; }
clickhouse_query() {
  docker exec sub2api-langfuse-clickhouse-1 clickhouse-client -u clickhouse --password clickhouse -q "$1"
}
admin_post() {
  local path="$1" payload="$2"
  curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d "$payload" "$BASE_URL$path"
}

for cmd in curl jq openssl docker; do
  command -v "$cmd" >/dev/null 2>&1 || fail "missing command: $cmd"
done

# 每个平台独立 group + API key，防止兼容平台之间互相抢占账号。
setup_platform() {
  local platform="$1" suffix="$2" credentials="$3" extra="$4" allow_image="$5"
  local group_payload group_id key_name api_key account_payload account_id
  group_payload=$(jq -nc --arg name "$PREFIX-$suffix" --arg platform "$platform" --argjson allow_image "$allow_image" '{
    name:$name,description:"model trace protocol matrix",platform:$platform,rate_multiplier:1,
    is_exclusive:false,status:"active",allow_image_generation:$allow_image,
    allow_messages_dispatch:($platform == "openai")
  }')
  group_id=$(admin_post /api/v1/admin/groups "$group_payload" | jq -er '.data.id')
  key_name="$PREFIX-$suffix-key"
  admin_post /api/v1/keys "$(jq -nc --arg name "$key_name" --argjson group_id "$group_id" '{name:$name,group_id:$group_id}')" >/dev/null
  api_key="sk-${PREFIX}-${suffix}-$(openssl rand -hex 8)"
  docker exec sub2api-deps-postgres-1 psql -U sub2api -d sub2api \
    -v ON_ERROR_STOP=1 -c "UPDATE api_keys SET key='$api_key' WHERE name='$key_name';" >/dev/null
  account_payload=$(jq -nc \
    --arg name "$PREFIX-$suffix-account" --arg platform "$platform" \
    --argjson group_id "$group_id" --argjson credentials "$credentials" --argjson extra "$extra" '{
      name:$name,platform:$platform,type:"apikey",concurrency:4,priority:1,status:"active",schedulable:true,
      group_ids:[$group_id],credentials:$credentials,extra:$extra
    }')
  account_id=$(admin_post /api/v1/admin/accounts "$account_payload" | jq -er '.data.id')
  printf '%s|%s|%s\n' "$group_id" "$api_key" "$account_id"
}

ANTHROPIC_SECRET="matrix-anthropic-secret-$RUN_ID"
OPENAI_SECRET="matrix-openai-secret-$RUN_ID"
GROK_SECRET="matrix-grok-secret-$RUN_ID"
GEMINI_SECRET="matrix-gemini-secret-$RUN_ID"
ANTIGRAVITY_SECRET="matrix-antigravity-secret-$RUN_ID"

IFS='|' read -r ANTHROPIC_GROUP ANTHROPIC_KEY ANTHROPIC_ACCOUNT < <(setup_platform anthropic anthropic \
  "$(jq -nc --arg secret "$ANTHROPIC_SECRET" '{api_key:$secret,base_url:"http://127.0.0.1:18081/matrix/anthropic",model_mapping:{"claude-e2e-matrix":"claude-e2e-upstream"}}')" '{}' false)
IFS='|' read -r OPENAI_GROUP OPENAI_KEY OPENAI_ACCOUNT < <(setup_platform openai openai \
  "$(jq -nc --arg secret "$OPENAI_SECRET" '{api_key:$secret,base_url:"http://127.0.0.1:18081/matrix/openai",openai_capabilities:["chat_completions","embeddings","alpha_search"],model_mapping:{"gpt-e2e-matrix":"gpt-e2e-upstream","embed-e2e-matrix":"embed-e2e-upstream","gpt-image-2":"gpt-image-e2e-upstream"}}')" \
  '{"openai_responses_supported":true,"openai_responses_mode":"force_responses"}' true)
IFS='|' read -r GROK_GROUP GROK_KEY GROK_ACCOUNT < <(setup_platform grok grok \
  "$(jq -nc --arg secret "$GROK_SECRET" '{api_key:$secret,base_url:"http://127.0.0.1:18081/matrix/grok",model_mapping:{"grok-imagine":"grok-imagine","grok-imagine-image-quality":"grok-imagine-image-quality","grok-imagine-edit":"grok-imagine-edit","grok-imagine-video":"grok-imagine-video"}}')" \
  '{"grok_media_eligible":true}' true)
IFS='|' read -r GEMINI_GROUP GEMINI_KEY GEMINI_ACCOUNT < <(setup_platform gemini gemini \
  "$(jq -nc --arg secret "$GEMINI_SECRET" '{api_key:$secret,base_url:"http://127.0.0.1:18081/matrix/gemini",model_mapping:{"gemini-e2e-matrix":"gemini-e2e-upstream"}}')" '{}' false)
IFS='|' read -r ANTIGRAVITY_GROUP ANTIGRAVITY_KEY ANTIGRAVITY_ACCOUNT < <(setup_platform antigravity antigravity \
  "$(jq -nc --arg secret "$ANTIGRAVITY_SECRET" '{api_key:$secret,base_url:"http://127.0.0.1:18081/matrix/gemini",model_mapping:{"gemini-e2e-matrix":"gemini-e2e-upstream","claude-e2e-matrix":"gemini-e2e-upstream"}}')" '{}' false)

log "accounts ready: anthropic=$ANTHROPIC_ACCOUNT openai=$OPENAI_ACCOUNT grok=$GROK_ACCOUNT gemini=$GEMINI_ACCOUNT antigravity=$ANTIGRAVITY_ACCOUNT"

CASE_COUNT=0
EXPECTED_MAP="$TMP_DIR/expected.tsv"
: >"$EXPECTED_MAP"
record_case() {
  local request_id="$1" protocol="$2"
  printf '%s\t%s\n' "$request_id" "$protocol" >>"$EXPECTED_MAP"
  CASE_COUNT=$((CASE_COUNT + 1))
}
post_json() {
  local label="$1" key="$2" path="$3" protocol="$4" payload="$5" auth_header="${6:-Authorization}"
  local request_id="$PREFIX-$label" response="$TMP_DIR/$label.json" code
  code=$(curl -sS -X POST "$BASE_URL$path" \
    -H "Content-Type: application/json" -H "$auth_header: $([[ "$auth_header" == "Authorization" ]] && printf 'Bearer ' )$key" \
    -H "X-Client-Request-ID: $request_id" -d "$payload" -o "$response" -w '%{http_code}')
  [[ "$code" == "200" ]] || { cat "$response" >&2; fail "$label returned HTTP $code"; }
  jq -e . "$response" >/dev/null || { cat "$response" >&2; fail "$label returned invalid JSON"; }
  record_case "$request_id" "$protocol"
}

CANARY="matrix-canary-$RUN_ID"
post_json anthropic "$ANTHROPIC_KEY" /v1/messages anthropic.messages \
  "$(jq -nc --arg prompt "$CANARY-anthropic" '{model:"claude-e2e-matrix",max_tokens:32,messages:[{role:"user",content:$prompt}]}')"
post_json chat "$OPENAI_KEY" /v1/chat/completions openai.chat_completions \
  "$(jq -nc --arg prompt "$CANARY-chat" '{model:"gpt-e2e-matrix",messages:[{role:"user",content:$prompt}]}')"
post_json responses "$OPENAI_KEY" /v1/responses openai.responses \
  "$(jq -nc --arg prompt "$CANARY-responses" '{model:"gpt-e2e-matrix",input:$prompt,stream:false}')"
post_json embeddings "$OPENAI_KEY" /v1/embeddings openai.embeddings \
  "$(jq -nc --arg prompt "$CANARY-embeddings" '{model:"embed-e2e-matrix",input:$prompt}')"
post_json search "$OPENAI_KEY" /v1/alpha/search openai.search \
  "$(jq -nc --arg prompt "$CANARY-search" '{model:"gpt-e2e-matrix",query:$prompt}')"
post_json anthropic-count "$ANTHROPIC_KEY" /v1/messages/count_tokens anthropic.count_tokens \
  "$(jq -nc --arg prompt "$CANARY-anthropic-count" '{model:"claude-e2e-matrix",messages:[{role:"user",content:$prompt}]}')"
post_json openai-backed-count "$OPENAI_KEY" /v1/messages/count_tokens anthropic.count_tokens \
  "$(jq -nc --arg prompt "$CANARY-openai-backed-count" '{model:"gpt-e2e-matrix",messages:[{role:"user",content:$prompt}]}')"
post_json openai-image-generation "$OPENAI_KEY" /v1/images/generations openai.images.generations \
  "$(jq -nc --arg prompt "$CANARY-openai-image-generation" '{model:"gpt-image-2",prompt:$prompt,size:"1024x1024"}')"

printf '\x89PNG\r\n\x1a\n' >"$TMP_DIR/input.png"
OPENAI_EDIT_ID="$PREFIX-openai-image-edit"
OPENAI_EDIT_CODE=$(curl -sS -X POST "$BASE_URL/v1/images/edits" \
  -H "Authorization: Bearer $OPENAI_KEY" -H "X-Client-Request-ID: $OPENAI_EDIT_ID" \
  -F 'model=gpt-image-2' -F "prompt=$CANARY-openai-image-edit" -F "image=@$TMP_DIR/input.png;type=image/png" \
  -o "$TMP_DIR/openai-image-edit.json" -w '%{http_code}')
[[ "$OPENAI_EDIT_CODE" == "200" ]] || { cat "$TMP_DIR/openai-image-edit.json" >&2; fail "openai image edit returned HTTP $OPENAI_EDIT_CODE"; }
jq -e . "$TMP_DIR/openai-image-edit.json" >/dev/null || fail "openai image edit returned invalid JSON"
record_case "$OPENAI_EDIT_ID" openai.images.edits

post_json grok-image-generation "$GROK_KEY" /v1/images/generations openai.images.generations \
  "$(jq -nc --arg prompt "$CANARY-grok-image-generation" '{model:"grok-imagine",prompt:$prompt}')"
post_json grok-image-edit "$GROK_KEY" /v1/images/edits openai.images.edits \
  "$(jq -nc --arg prompt "$CANARY-grok-image-edit" '{model:"grok-imagine-edit",prompt:$prompt,image:{url:"https://example.test/source.png"}}')"
post_json grok-video-generation "$GROK_KEY" /v1/videos/generations openai.videos.generations \
  "$(jq -nc --arg prompt "$CANARY-grok-video-generation" '{model:"grok-imagine-video",prompt:$prompt,resolution:"480p",duration:1}')"
post_json grok-video-edit "$GROK_KEY" /v1/videos/edits openai.videos.edits \
  "$(jq -nc --arg prompt "$CANARY-grok-video-edit" '{model:"grok-imagine-video",prompt:$prompt,video:{url:"https://example.test/source.mp4"}}')"
post_json grok-video-extension "$GROK_KEY" /v1/videos/extensions openai.videos.extensions \
  "$(jq -nc --arg prompt "$CANARY-grok-video-extension" '{model:"grok-imagine-video",prompt:$prompt,video:{url:"https://example.test/source.mp4"}}')"
post_json gemini "$GEMINI_KEY" '/v1beta/models/gemini-e2e-matrix:generateContent' gemini.generateContent \
  "$(jq -nc --arg prompt "$CANARY-gemini" '{contents:[{role:"user",parts:[{text:$prompt}]}]}')" x-goog-api-key

GEMINI_STREAM_ID="$PREFIX-gemini-stream"
GEMINI_STREAM_CODE=$(curl -sS -X POST "$BASE_URL/v1beta/models/gemini-e2e-matrix:streamGenerateContent?alt=sse" \
  -H "Content-Type: application/json" -H "x-goog-api-key: $GEMINI_KEY" -H "X-Client-Request-ID: $GEMINI_STREAM_ID" \
  -d "$(jq -nc --arg prompt "$CANARY-gemini-stream" '{contents:[{role:"user",parts:[{text:$prompt}]}]}')" \
  -o "$TMP_DIR/gemini-stream.sse" -w '%{http_code}')
[[ "$GEMINI_STREAM_CODE" == "200" ]] || { cat "$TMP_DIR/gemini-stream.sse" >&2; fail "Gemini stream returned HTTP $GEMINI_STREAM_CODE"; }
grep -q 'matrix gemini stream' "$TMP_DIR/gemini-stream.sse" || fail "Gemini stream missing upstream content"
record_case "$GEMINI_STREAM_ID" gemini.streamGenerateContent

post_json antigravity-native "$ANTIGRAVITY_KEY" /antigravity/v1/messages anthropic.messages \
  "$(jq -nc --arg prompt "$CANARY-antigravity-native" '{model:"claude-e2e-matrix",max_tokens:32,messages:[{role:"user",content:$prompt}]}')"
post_json antigravity-gemini "$ANTIGRAVITY_KEY" '/antigravity/v1beta/models/gemini-e2e-matrix:generateContent' gemini.generateContent \
  "$(jq -nc --arg prompt "$CANARY-antigravity-gemini" '{contents:[{role:"user",parts:[{text:$prompt}]}]}')" x-goog-api-key

sort -o "$EXPECTED_MAP" "$EXPECTED_MAP"
log "business responses verified: cases=$CASE_COUNT"

# 上游夹具必须看到每种实际协议；这同时证明账号 base_url、认证与协议转换走到了真实 HTTP 边界。
FIXTURE_STATS=$(curl -fsS http://127.0.0.1:18081/stats)
echo "$FIXTURE_STATS" | jq -e '
  .protocol["anthropic.messages"] >= 1 and
  .protocol["openai.chat_completions"] >= 1 and
  .protocol["openai.responses"] >= 1 and
  .protocol["openai.embeddings"] >= 1 and
  .protocol["openai.search"] >= 1 and
  .protocol["anthropic.count_tokens"] >= 1 and
  .protocol["openai.count_tokens"] >= 1 and
  .protocol["openai.images.generations"] >= 2 and
  .protocol["openai.images.edits"] >= 2 and
  .protocol["openai.videos.generations"] >= 1 and
  .protocol["openai.videos.edits"] >= 1 and
  .protocol["openai.videos.extensions"] >= 1 and
  .protocol["gemini.generateContent"] >= 1 and
  .protocol["gemini.streamGenerateContent"] >= 1 and
  .protocol["antigravity.generateContent"] >= 1
' >/dev/null || { echo "$FIXTURE_STATS" | jq . >&2; fail "upstream protocol coverage mismatch"; }

# 等待 BatchSpanProcessor + Langfuse ingestion。
TRACE_COUNT=0
for _ in {1..30}; do
  TRACE_COUNT=$(clickhouse_query "SELECT count() FROM traces WHERE startsWith(metadata['request_id'], '$PREFIX-') FORMAT TabSeparated" 2>/dev/null || true)
  [[ "$TRACE_COUNT" == "$CASE_COUNT" ]] && break
  sleep 1
done
[[ "$TRACE_COUNT" == "$CASE_COUNT" ]] || fail "Langfuse trace count=$TRACE_COUNT, want $CASE_COUNT"

ACTUAL_MAP="$TMP_DIR/actual.tsv"
clickhouse_query "SELECT metadata['request_id'], metadata['entry_protocol'] FROM traces WHERE startsWith(metadata['request_id'], '$PREFIX-') ORDER BY metadata['request_id'] FORMAT TabSeparated" >"$ACTUAL_MAP"
cmp -s "$EXPECTED_MAP" "$ACTUAL_MAP" || { diff -u "$EXPECTED_MAP" "$ACTUAL_MAP" >&2 || true; fail "request-to-protocol mapping mismatch"; }

TRACE_ROW=$(clickhouse_query "SELECT count(), uniqExact(id), uniqExact(metadata['request_id']), countIf(name = 'model.request'), countIf(user_id = ''), countIf(metadata['api_key_id'] = ''), countIf(metadata['group_id'] = ''), countIf(NOT has(tags, concat('entry_protocol:', metadata['entry_protocol']))) FROM traces WHERE startsWith(metadata['request_id'], '$PREFIX-') FORMAT TabSeparated")
IFS=$'\t' read -r T_COUNT T_IDS T_REQUESTS T_ROOT_NAMES T_EMPTY_USER T_EMPTY_KEY T_EMPTY_GROUP T_MISSING_TAG <<<"$TRACE_ROW"
[[ "$T_COUNT" == "$CASE_COUNT" && "$T_IDS" == "$CASE_COUNT" && "$T_REQUESTS" == "$CASE_COUNT" && "$T_ROOT_NAMES" == "$CASE_COUNT" ]] \
  || fail "trace identity/cardinality mismatch: $TRACE_ROW"
[[ "$T_EMPTY_USER" == "0" && "$T_EMPTY_KEY" == "0" && "$T_EMPTY_GROUP" == "0" && "$T_MISSING_TAG" == "0" ]] \
  || fail "trace identity/tag metadata incomplete: $TRACE_ROW"

OBS_ROW=$(clickhouse_query "SELECT countIf(name = 'model.request' AND type = 'SPAN'), countIf(type = 'GENERATION'), countIf(end_time IS NULL), countIf(level = 'ERROR'), countIf(position(input, '$CANARY') > 0) FROM observations WHERE trace_id IN (SELECT id FROM traces WHERE startsWith(metadata['request_id'], '$PREFIX-')) FORMAT TabSeparated")
IFS=$'\t' read -r ROOT_COUNT GENERATION_COUNT UNFINISHED_COUNT ERROR_COUNT CANARY_INPUTS <<<"$OBS_ROW"
[[ "$ROOT_COUNT" == "$CASE_COUNT" && "$GENERATION_COUNT" == "$CASE_COUNT" && "$UNFINISHED_COUNT" == "0" && "$ERROR_COUNT" == "0" ]] \
  || fail "observation hierarchy/status mismatch: roots=$ROOT_COUNT generations=$GENERATION_COUNT unfinished=$UNFINISHED_COUNT errors=$ERROR_COUNT"
(( CANARY_INPUTS >= CASE_COUNT )) || fail "trace inputs lost client prompts: matches=$CANARY_INPUTS cases=$CASE_COUNT"

# 已知 usage 的协议逐项对齐上游事实；total cost 字段必须存在（允许免费测试组为 0）。
assert_usage() {
  local label="$1" input="$2" output="$3" total="$4" row
  row=$(clickhouse_query "SELECT countIf(usage_details['input'] = $input AND usage_details['output'] = $output AND usage_details['total'] = $total AND mapContains(cost_details, 'total')) FROM observations WHERE trace_id IN (SELECT id FROM traces WHERE metadata['request_id'] = '$PREFIX-$label') AND type = 'GENERATION' FORMAT TabSeparated")
  [[ "$row" == "1" ]] || fail "$label usage/cost mismatch: got=$row want=1 ($input/$output/$total)"
}
assert_usage anthropic 11 3 14
assert_usage chat 12 4 16
assert_usage responses 13 5 18
assert_usage embeddings 3 0 3
assert_usage gemini 17 4 21
assert_usage gemini-stream 17 4 21
assert_usage antigravity-native 17 4 21
assert_usage antigravity-gemini 17 4 21

for secret in "$ANTHROPIC_SECRET" "$OPENAI_SECRET" "$GROK_SECRET" "$GEMINI_SECRET" "$ANTIGRAVITY_SECRET" "$ANTHROPIC_KEY" "$OPENAI_KEY" "$GROK_KEY" "$GEMINI_KEY" "$ANTIGRAVITY_KEY"; do
  LEAKS=$(clickhouse_query "SELECT (SELECT count() FROM traces WHERE startsWith(metadata['request_id'], '$PREFIX-') AND position(toString(metadata), '$secret') > 0) + (SELECT count() FROM observations WHERE trace_id IN (SELECT id FROM traces WHERE startsWith(metadata['request_id'], '$PREFIX-')) AND (position(input, '$secret') > 0 OR position(output, '$secret') > 0 OR position(toString(metadata), '$secret') > 0)) FORMAT TabSeparated")
  [[ "$LEAKS" == "0" ]] || fail "credential canary leaked into Langfuse: matches=$LEAKS"
done

log "Langfuse protocol matrix verified: traces=$T_COUNT roots=$ROOT_COUNT generations=$GENERATION_COUNT"
printf 'MATRIX_TRACE_COUNT=%s\nMATRIX_GENERATION_COUNT=%s\nPROTOCOL_MATRIX_VERIFY_OK\n' "$T_COUNT" "$GENERATION_COUNT"
