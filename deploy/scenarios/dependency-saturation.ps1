$ErrorActionPreference = "Stop"
kubectl -n optiscale-demo set env deployment/demo-api DEMO_WORK_MS=10 DEMO_CONCURRENCY=40
kubectl -n optiscale-demo set env deployment/inventory-service INVENTORY_WORK_MS=400 INVENTORY_CONCURRENCY=4
kubectl -n optiscale-demo set env deployment/demo-loadgen LOADGEN_CONCURRENCY=20
kubectl -n optiscale-demo rollout status deployment/demo-api
kubectl -n optiscale-demo rollout status deployment/inventory-service
Write-Host "Dependency-saturation profile applied. Allow at least 60 seconds for p95 and averaged utilization observations."
