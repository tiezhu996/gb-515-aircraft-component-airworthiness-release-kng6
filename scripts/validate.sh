#!/usr/bin/env sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
set -a
if [ -f .env ]; then . ./.env; else . ./.env.example; fi
set +a

(command -v jq >/dev/null 2>&1) || { echo "jq is required for API validation" >&2; exit 1; }

(cd backend && go test ./... && go test -race ./... && go vet ./... && go build ./...)
(cd frontend && npm install --no-audit --no-fund && npm run typecheck && npm run build)
docker compose config --quiet
docker compose down -v --remove-orphans
docker compose up -d --build

cleanup() { docker compose down -v --remove-orphans; }
if [ "${KEEP_RUNNING:-0}" = "1" ]; then
  trap cleanup INT TERM
else
  trap cleanup EXIT INT TERM
fi

i=0
until curl -fsS "http://127.0.0.1:${BACKEND_PORT:-19515}/healthz" | jq -e '.data.status == "ok" and .data.database == "ready" and .data.redis == "ready"' >/dev/null; do
  i=$((i+1))
  [ "$i" -lt 60 ] || { docker compose logs; exit 1; }
  sleep 2
done
curl -fsS "http://127.0.0.1:${FRONTEND_PORT:-18515}/" >/dev/null

login_token() {
  curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT:-19515}/api/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$1\",\"password\":\"Admin123!\"}" | jq -er '.data.token'
}

admin_token=$(login_token admin)
reviewer_token=$(login_token reviewer)
operator_token=$(login_token operator)
viewer_token=$(login_token viewer)

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/session" -H "Authorization: Bearer $viewer_token" \
  | jq -e '.data.role == "viewer" and (.data.requestId | length > 0)' >/dev/null
curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/parts?page=1&pageSize=20" -H "Authorization: Bearer $viewer_token" \
  | jq -e '.data | length >= 3' >/dev/null

viewer_write_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts" \
  -H "Authorization: Bearer $viewer_token" -H 'Content-Type: application/json' -d '{}')
[ "$viewer_write_status" = "403" ]

now=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
suffix=$(date +%s)

# --- Linked part must exist and be releasable before submit/approve ---
part_payload=$(printf '{"code":"PART-SMOKE-%s","name":"Smoke linked component","description":"Linkage validation part","facility":"Validation Hangar","owner":"Release Desk","category":"engine","riskLevel":"high","metricValue":100,"metricUnit":"percent","effectiveAt":"%s","evidence":"inspection IR-SMOKE","relatedCode":"PLAN-SMOKE"}' "$suffix" "$now")
part=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-part-create' \
  -d "$part_payload")
part_id=$(printf '%s' "$part" | jq -er '.data.id')
part_version=$(printf '%s' "$part" | jq -er '.data.version')
part_code=$(printf '%s' "$part" | jq -er '.data.code')

# A held part blocks submit: authorization keeps draft v1 and the checkpoint
# (part code/status/block reason) is readable back.
held_part_payload=$(printf '{"code":"PART-HELD-%s","name":"Smoke held component","description":"Linkage block part","facility":"Validation Hangar","owner":"Release Desk","category":"engine","riskLevel":"critical","metricValue":0,"metricUnit":"percent","effectiveAt":"%s","evidence":"quarantined","relatedCode":"PLAN-HELD"}' "$suffix" "$now")
held_part=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -d "$held_part_payload")
held_part_id=$(printf '%s' "$held_part" | jq -er '.data.id')
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${held_part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -d '{"status":"inspection","expectedVersion":1,"reason":"route to hold"}' >/dev/null
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${held_part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -d '{"status":"hold","expectedVersion":2,"reason":"quality quarantine"}' >/dev/null

blocked_payload=$(printf '{"code":"AUTH-HELD-%s","name":"Blocked component release","description":"Linkage block validation","facility":"Validation Hangar","owner":"Release Desk","category":"engine","riskLevel":"high","metricValue":100,"metricUnit":"percent","effectiveAt":"%s","evidence":"held link","relatedCode":"PART-HELD-%s"}' "$suffix" "$now" "$suffix")
blocked_auth=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -d "$blocked_payload")
blocked_auth_id=$(printf '%s' "$blocked_auth" | jq -er '.data.id')
block_status=$(curl -sS -o /tmp/gb515-block.json -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${blocked_auth_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-blocked' \
  -d '{"status":"review","expectedVersion":1,"reason":"submit against held part"}')
[ "$block_status" = "409" ]
jq -e --arg s "$suffix" '.error == "part_linkage_blocked" and .details.partCode == ("PART-HELD-" + $s) and .details.partStatus == "hold"' /tmp/gb515-block.json >/dev/null
curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${blocked_auth_id}" -H "Authorization: Bearer $admin_token" \
  | jq -e '.data.status == "draft" and .data.version == 1 and .data.linkageResult == "blocked" and .data.relatedPartStatus == "hold" and (.data.blockReason | length > 0) and (.data.revisions | length) == 1' >/dev/null

authorization_payload=$(printf '{"code":"AUTH-SMOKE-%s","name":"Validated component release","description":"Dual-control Compose validation","facility":"Validation Hangar","owner":"Release Desk","category":"engine","riskLevel":"high","metricValue":100,"metricUnit":"percent","effectiveAt":"%s","evidence":"inspection IR-SMOKE and certificate CERT-SMOKE","relatedCode":"%s"}' "$suffix" "$now" "$part_code")
authorization=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-create' \
  -d "$authorization_payload")
authorization_id=$(printf '%s' "$authorization" | jq -er '.data.id')
authorization_version=$(printf '%s' "$authorization" | jq -er '.data.version')
printf '%s' "$authorization" | jq -e '.data.status == "draft" and .data.version == 1 and .data.revisions[0].actor == "operator" and .data.revisions[0].requestId == "gb515-auth-create"' >/dev/null

authorization_review=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-review' \
  -d "{\"status\":\"review\",\"expectedVersion\":${authorization_version},\"reason\":\"inspection and certificate evidence complete\"}")
authorization_review_version=$(printf '%s' "$authorization_review" | jq -er '.data.version')
printf '%s' "$authorization_review" | jq -e '.data.status == "review" and .data.submittedBy == "operator" and (.data.revisions | length) == 2 and .data.linkageResult == "clear" and .data.relatedPartStatus == "received"' >/dev/null

operator_approval_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-operator-denied' \
  -d "{\"status\":\"approved\",\"expectedVersion\":${authorization_review_version},\"reason\":\"operator must not self approve\"}")
[ "$operator_approval_status" = "403" ]

authorization_approved=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}/transition" \
  -H "Authorization: Bearer $reviewer_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-approve' \
  -d "{\"status\":\"approved\",\"expectedVersion\":${authorization_review_version},\"reason\":\"independent airworthiness release review passed\"}")
printf '%s' "$authorization_approved" | jq -e '.data.status == "approved" and .data.version == 3 and .data.submittedBy == "operator" and .data.reviewedBy == "reviewer" and (.data.revisions | length) == 3 and .data.revisions[2].requestId == "gb515-auth-approve"' >/dev/null

# After approval the linked part enters hold: the authorization must be
# revoked in the same transaction (v4), keeping the approved v3 revision.
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-part-inspect' \
  -d "{\"status\":\"inspection\",\"expectedVersion\":${part_version},\"reason\":\"inspect before hold\"}" >/dev/null
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-part-hold' \
  -d '{"status":"hold","expectedVersion":2,"reason":"post approval quality freeze"}' >/dev/null
curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}" -H "Authorization: Bearer $admin_token" \
  | jq -e '.data.status == "revoked" and .data.version == 4 and .data.linkageResult == "revoked" and (.data.revisions | length) == 4 and .data.revisions[2].status == "approved" and .data.revisions[3].status == "revoked" and .data.revisions[3].requestId == "gb515-part-hold"' >/dev/null
# A stale concurrent hold must be rejected (invalid duplicate transition or
# optimistic-lock conflict) and must not create a second revocation.
stale_hold_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d '{"status":"hold","expectedVersion":2,"reason":"stale concurrent freeze"}')
[ "$stale_hold_status" = "409" ] || [ "$stale_hold_status" = "422" ]
curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}" -H "Authorization: Bearer $admin_token" \
  | jq -e '.data.status == "revoked" and .data.version == 4 and (.data.revisions | length) == 4' >/dev/null

certificate_payload=$(printf '{"code":"CERT-SMOKE-%s","name":"Validated airworthiness certificate","description":"Immutable certificate validation","facility":"Validation Hangar","owner":"Certificate Desk","category":"engine","riskLevel":"medium","metricValue":100,"metricUnit":"percent","effectiveAt":"%s","evidence":"inspection report IR-SMOKE","relatedCode":"PART-SMOKE"}' "$suffix" "$now")
certificate=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/certificates" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-cert-create' \
  -d "$certificate_payload")
certificate_id=$(printf '%s' "$certificate" | jq -er '.data.id')
certificate_version=$(printf '%s' "$certificate" | jq -er '.data.version')

operator_certificate_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/certificates/${certificate_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-cert-operator-denied' \
  -d "{\"status\":\"valid\",\"expectedVersion\":${certificate_version},\"reason\":\"operator must not publish\"}")
[ "$operator_certificate_status" = "403" ]

certificate_valid=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/certificates/${certificate_id}/transition" \
  -H "Authorization: Bearer $reviewer_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-cert-publish' \
  -d "{\"status\":\"valid\",\"expectedVersion\":${certificate_version},\"reason\":\"independent certificate evidence review passed\"}")
printf '%s' "$certificate_valid" | jq -e '.data.status == "valid" and .data.version == 2 and .data.preparedBy == "operator" and .data.verifiedBy == "reviewer" and (.data.revisions | length) == 2 and .data.revisions[1].requestId == "gb515-cert-publish"' >/dev/null

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/audits/ReleaseAuthorization/${authorization_id}?limit=10" \
  -H "Authorization: Bearer $admin_token" \
  | jq -e '[.data[].requestId] | index("gb515-auth-create") != null and index("gb515-auth-review") != null and index("gb515-auth-approve") != null and index("gb515-part-hold") != null' >/dev/null
curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/audit-summary?windowHours=24" -H "Authorization: Bearer $admin_token" \
  | jq -e '.data.total >= 5 and .data.transitions >= 3 and .data.uniqueActors >= 2' >/dev/null

docker compose ps
[ "${KEEP_RUNNING:-0}" = "1" ] && echo "KEEP_RUNNING=1: containers left running for built-in Browser validation"
