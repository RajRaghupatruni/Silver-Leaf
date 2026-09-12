$ErrorActionPreference = "Stop"

# Keep both application tiers locally unsaturated while inventory calls wait on
# an intentionally slow query executed by the real local PostgreSQL pod.
kubectl -n optiscale-demo set env deployment/demo-api DEMO_WORK_MS=10 DEMO_CONCURRENCY=40
kubectl -n optiscale-demo set env deployment/inventory-service INVENTORY_WORK_MS=10 INVENTORY_CONCURRENCY=40 INVENTORY_DB_DELAY_MS=800 DATABASE_MAX_OPEN_CONNS=40
kubectl -n optiscale-demo set env deployment/demo-loadgen LOADGEN_CONCURRENCY=40

kubectl -n optiscale-demo rollout status deployment/postgres
kubectl -n optiscale-demo rollout status deployment/inventory-service
kubectl -n optiscale-demo rollout status deployment/demo-api
Write-Host "Database-bottleneck profile applied. Allow at least 60 seconds for p95 and averaged active-query observations. Expected: CAPACITY_BLOCKED / HOLD; neither demo-api nor inventory-service should scale."
