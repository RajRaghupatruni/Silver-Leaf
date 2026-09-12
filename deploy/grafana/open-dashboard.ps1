$ErrorActionPreference = "Stop"

$namespace = "optiscale-system"
$serviceName = "grafana"
$localPort = 3000

$deploymentJson = & kubectl -n $namespace get deployment grafana -o json
if ($LASTEXITCODE -ne 0) {
    throw "Grafana Deployment is unavailable in namespace $namespace. Run make install first."
}
$deployment = $deploymentJson | ConvertFrom-Json
if ([int]$deployment.status.availableReplicas -lt 1) {
    & kubectl -n $namespace rollout status deployment/grafana --timeout=120s
    if ($LASTEXITCODE -ne 0) {
        throw "Grafana did not become available. Inspect deployment/grafana and its pod events."
    }
}

$endpointJson = & kubectl -n $namespace get endpoints $serviceName -o json
if ($LASTEXITCODE -ne 0) {
    throw "Could not read Grafana Service endpoints in namespace $namespace."
}
$endpoints = $endpointJson | ConvertFrom-Json
$readyAddressCount = 0
foreach ($subset in $endpoints.subsets) {
    $readyAddressCount += @($subset.addresses).Count
}
if ($readyAddressCount -lt 1) {
    throw "Grafana Service has no ready endpoints. Check the Grafana Deployment and readiness probe."
}

Write-Host "Grafana is available. Forwarding 127.0.0.1:$localPort to service/$serviceName`:3000 in namespace $namespace."
Write-Host "Open http://localhost:$localPort and select the OptiScale Capacity Governor dashboard."
Write-Host "Anonymous Viewer access is intended only for this loopback-bound local demo. Press Ctrl+C to stop forwarding."
& kubectl -n $namespace port-forward --address 127.0.0.1 "service/$serviceName" "$($localPort):3000"
if ($LASTEXITCODE -ne 0) {
    throw "kubectl port-forward exited with code $LASTEXITCODE. Check whether localhost:$localPort is already in use."
}
