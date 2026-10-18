#!/usr/bin/env bash
# One-time SonarQube setup for the demo stack: waits for SonarQube, replaces
# the default admin password and prints an analysis token for .env.
#   deploy/stack/init-sonar.sh [sonar-url]
set -euo pipefail
SONAR="${1:-http://localhost:9000}"
NEW_PASSWORD="${SONAR_ADMIN_PASSWORD:-Repoguard-$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 12)-1!}"

echo "Waiting for SonarQube at $SONAR ..." >&2
for _ in $(seq 1 120); do
  if curl -fsS "$SONAR/api/system/status" 2>/dev/null | grep -q '"status":"UP"'; then break; fi
  sleep 5
done
curl -fsS "$SONAR/api/system/status" | grep -q '"status":"UP"' || { echo "SonarQube did not come up" >&2; exit 1; }

PASSWORD=admin
if curl -fsS -u "admin:admin" -X POST "$SONAR/api/users/change_password" \
     --data-urlencode "login=admin" --data-urlencode "previousPassword=admin" \
     --data-urlencode "password=$NEW_PASSWORD" >/dev/null 2>&1; then
  PASSWORD="$NEW_PASSWORD"
  echo "SonarQube admin password changed to: $NEW_PASSWORD  (save it)" >&2
elif [ -n "${SONAR_ADMIN_PASSWORD:-}" ]; then
  PASSWORD="$SONAR_ADMIN_PASSWORD"   # already initialised earlier
fi

TOKEN=$(curl -fsS -u "admin:$PASSWORD" -X POST "$SONAR/api/user_tokens/generate" \
  --data-urlencode "name=repoguard-$(date +%s)" --data-urlencode "type=GLOBAL_ANALYSIS_TOKEN" \
  | sed -E 's/.*"token":"([^"]+)".*/\1/')
[ -n "$TOKEN" ] || { echo "could not create a token" >&2; exit 1; }
echo "$TOKEN"
echo "Put this in deploy/stack/.env as SONAR_TOKEN=..., then: docker compose -f deploy/stack/docker-compose.yml up -d" >&2
