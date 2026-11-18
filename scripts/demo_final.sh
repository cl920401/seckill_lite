#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

BASE_URL="${BASE_URL:-http://127.0.0.1:18080}"
ADMIN_TOKEN="${ADMIN_TOKEN:-demo-admin-token}"
MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_USER="${MYSQL_USER:-root}"
MYSQL_DB="${MYSQL_DB:-seckill_test}"
MYSQL_DSN="${MYSQL_DSN:-root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4}"
REDIS_DB="${REDIS_DB:-14}"
REDIS_PREFIX="${REDIS_PREFIX:-sk:demo:final:}"
RUN_TAG="$(date +%s)"

say() { printf '\n== %s ==\n' "$*"; }
mysqlq() { mysql -h "$MYSQL_HOST" -P "$MYSQL_PORT" -u "$MYSQL_USER" -N -e "$1" "$MYSQL_DB"; }

create_activity() {
  local name="$1" stock="$2" limit="$3"
  curl -sS -X POST "$BASE_URL/api/v1/admin/activities" \
    -H "X-Admin-Token: $ADMIN_TOKEN" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"startAt\":\"2020-01-01T00:00:00Z\",\"endAt\":\"2030-01-01T00:00:00Z\",\"items\":[{\"skuCode\":\"$name-SKU\",\"name\":\"$name item\",\"priceCents\":100,\"totalStock\":$stock,\"perUserLimit\":$limit}]}"
}

poll_request() {
  local user="$1" key="$2"
  for _ in $(seq 1 50); do
    local body code
    body="$(curl -sS -w '\n%{http_code}' "$BASE_URL/api/v1/seckill/requests/$key" -H "X-User-Id: $user")"
    code="$(printf '%s' "$body" | tail -n1)"
    body="$(printf '%s' "$body" | sed '$d')"
    if [ "$code" != "202" ]; then
      printf '%s' "$body"
      return 0
    fi
    sleep 0.2
  done
  echo "poll timeout for $key" >&2
  return 1
}

say "health/ready"
curl -sS "$BASE_URL/healthz"; echo
curl -sS "$BASE_URL/readyz"; echo

say "A. create activity + happy path + pay + idempotency conflict"
act_a="$(create_activity demo-a 5 1)"
item_a="$(printf '%s' "$act_a" | jq -r '.items[0].itemId')"
echo "activity=$act_a"
user_a=810001
key_a="demo-pay-$RUN_TAG"
buy_a="$(curl -sS -X POST "$BASE_URL/api/v1/seckill/orders" \
  -H "X-User-Id: $user_a" -H "Idempotency-Key: $key_a" -H 'Content-Type: application/json' \
  -d "{\"itemId\":$item_a,\"quantity\":1}")"
echo "buy=$buy_a"
status_a="$(poll_request "$user_a" "$key_a")"
echo "status=$status_a"
order_a="$(printf '%s' "$status_a" | jq -r '.order.orderNo')"
pay_a="$(curl -sS -X POST "$BASE_URL/api/v1/orders/$order_a/pay/mock" \
  -H "X-User-Id: $user_a" -H 'Content-Type: application/json' \
  -d "{\"providerTxnId\":\"txn-$RUN_TAG\",\"amountCents\":100}")"
echo "pay=$pay_a"
conflict="$(curl -sS -w '\n%{http_code}' -X POST "$BASE_URL/api/v1/seckill/orders" \
  -H "X-User-Id: $user_a" -H "Idempotency-Key: $key_a" -H 'Content-Type: application/json' \
  -d "{\"itemId\":$item_a,\"quantity\":2}")"
echo "conflict=$conflict"

say "B. concurrent buy: 200 requests / 50 stock"
act_b="$(create_activity demo-b 50 1)"
item_b="$(printf '%s' "$act_b" | jq -r '.items[0].itemId')"
act_b_id="$(printf '%s' "$act_b" | jq -r '.activityId')"
echo "activity=$act_b"
go run ./cmd/loadtest -base "$BASE_URL" -n 200 -c 50 -item "$item_b" -activity "$act_b_id" -mode unique -poll -dsn "$MYSQL_DSN"

say "C. timeout cancel restocks once"
act_c="$(create_activity demo-c 1 1)"
item_c="$(printf '%s' "$act_c" | jq -r '.items[0].itemId')"
echo "activity=$act_c"
user_c=810002
key_c="demo-cancel-$RUN_TAG"
buy_c="$(curl -sS -X POST "$BASE_URL/api/v1/seckill/orders" \
  -H "X-User-Id: $user_c" -H "Idempotency-Key: $key_c" -H 'Content-Type: application/json' \
  -d "{\"itemId\":$item_c,\"quantity\":1}")"
echo "buy=$buy_c"
status_c="$(poll_request "$user_c" "$key_c")"
order_c="$(printf '%s' "$status_c" | jq -r '.order.orderNo')"
echo "order=$order_c"
mysqlq "UPDATE orders SET expire_at = UTC_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE order_no = '$order_c';"
sleep 3
final_c="$(curl -sS "$BASE_URL/api/v1/orders/$order_c" -H "X-User-Id: $user_c")"
echo "final=$final_c"
echo "db_stock=$(mysqlq "SELECT available_stock FROM seckill_items WHERE id = $item_c;")"
echo "db_cancel_logs=$(mysqlq "SELECT COUNT(*) FROM stock_logs WHERE order_no = '$order_c' AND type = 'CANCEL';")"
echo "redis_stock=$(redis-cli -n "$REDIS_DB" GET "${REDIS_PREFIX}stock:$item_c")"
echo "redis_restocked_marker=$(redis-cli -n "$REDIS_DB" EXISTS "${REDIS_PREFIX}restocked:$order_c")"

say "done"
