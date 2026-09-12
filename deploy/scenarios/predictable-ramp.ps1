$ErrorActionPreference = "Stop"

$namespace = "optiscale-demo"
$controllerNamespace = "optiscale-system"
$k6JobName = "optiscale-predictable-ramp"
$k6ConfigMapName = "optiscale-k6-script"
$baselineRate = 200
$baselineSeconds = 65
$controllerWarmSeconds = 45
$rampSeconds = 180
$rampTargetRate = 480
$postRampSeconds = 45
$pollSeconds = 15
$reconcileIntervalSeconds = 15
$readinessDelayMs = 12000
$repoK6Script = (Resolve-Path (Join-Path $PSScriptRoot "..\k6\predictable-ramp.js")).Path
$repoK6Job = (Resolve-Path (Join-Path $PSScriptRoot "..\k6\predictable-ramp-job.yaml")).Path

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$KubectlArgs)

    $output = & kubectl @KubectlArgs
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($KubectlArgs -join ' ') failed with exit code $LASTEXITCODE."
    }
    return $output
}

function Get-KubectlJson {
    param([Parameter(Mandatory = $true)][string[]]$KubectlArgs)

    $jsonText = (Invoke-Kubectl -KubectlArgs $KubectlArgs) -join [Environment]::NewLine
    return ($jsonText | ConvertFrom-Json)
}

function Get-PrometheusScalar {
    param([Parameter(Mandatory = $true)][string]$Query)

    $encodedQuery = [uri]::EscapeDataString($Query)
    $path = "/api/v1/namespaces/$controllerNamespace/services/prometheus:9090/proxy/api/v1/query?query=$encodedQuery"
    try {
        $response = Get-KubectlJson -KubectlArgs @("get", "--raw", $path)
        if ($response.status -ne "success" -or $response.data.result.Count -eq 0) {
            return $null
        }
        return [double]::Parse([string]$response.data.result[0].value[1], [Globalization.CultureInfo]::InvariantCulture)
    }
    catch {
        return $null
    }
}

function Format-Value($Value) {
    if ($null -eq $Value -or [string]$Value -eq "") {
        return "n/a"
    }
    return ([double]$Value).ToString("0.##", [Globalization.CultureInfo]::InvariantCulture)
}

function Get-ScaleSignature($ResourceStatus) {
    $scaleTime = [string]$ResourceStatus.status.lastScaleTime
    $scaleDecision = $ResourceStatus.status.lastScaleDecision
    if ($null -eq $scaleDecision) {
        return "$scaleTime||"
    }
    return ("{0}|{1}|{2}" -f $scaleTime, [string]$scaleDecision.id, [string]$scaleDecision.timestamp)
}

function Get-OptiScaler {
    return (Get-KubectlJson -KubectlArgs @("-n", $namespace, "get", "optiscaler", "demo-api", "-o", "json"))
}

function Get-K6Job {
    return (Get-KubectlJson -KubectlArgs @("-n", $namespace, "get", "job", $k6JobName, "-o", "json"))
}

function Show-K6Logs {
    try {
        Invoke-Kubectl -KubectlArgs @("-n", $namespace, "logs", "job/$k6JobName", "--all-containers=true") | Out-Host
    }
    catch {
        Write-Warning "Could not retrieve k6 logs: $_"
    }
}

function Stop-K6AndFail {
    param([Parameter(Mandatory = $true)][string]$Reason)

    Show-K6Logs
    try {
        Invoke-Kubectl -KubectlArgs @("-n", $namespace, "delete", "job", $k6JobName, "--ignore-not-found=true", "--wait=true") | Out-Host
    }
    catch {
        Write-Warning "Could not stop k6 Job cleanly: $_"
    }
    throw $Reason
}

function Get-K6ContainerStartTime {
    param([int]$TimeoutSeconds = 180)

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $job = Get-K6Job
        if ([int]$job.status.failed -gt 0) {
            Show-K6Logs
            throw "k6 Job failed before its container started."
        }
        $pods = Get-KubectlJson -KubectlArgs @("-n", $namespace, "get", "pods", "-l", "job-name=$k6JobName", "-o", "json")
        foreach ($pod in $pods.items) {
            foreach ($container in $pod.status.containerStatuses) {
                if ($container.state.running.startedAt) {
                    return [DateTime]::Parse([string]$container.state.running.startedAt).ToUniversalTime()
                }
            }
        }
        Start-Sleep -Seconds 3
    }
    Stop-K6AndFail "Timed out waiting for the k6 container to start. Check image pull and Job pod events."
}

function Get-ReadyTargetReplicas {
    $deployment = Get-KubectlJson -KubectlArgs @("-n", $namespace, "get", "deployment", "demo-api", "-o", "json")
    return [int]$deployment.status.readyReplicas
}

function Get-PreScaleEvent {
    param([DateTime]$NotBefore)

    # kubectl may expose either core/v1 Events (involvedObject) or events.k8s.io/v1 (regarding).
    $events = Get-KubectlJson -KubectlArgs @("-n", $namespace, "get", "events", "-o", "json")
    foreach ($event in $events.items) {
        if ($event.reason -ne "PreScaleTarget") {
            continue
        }
        $objectName = [string]$event.involvedObject.name
        $objectKind = [string]$event.involvedObject.kind
        if (-not $objectName) { $objectName = [string]$event.regarding.name }
        if (-not $objectKind) { $objectKind = [string]$event.regarding.kind }
        if ($objectName -ne "demo-api" -or $objectKind -ne "OptiScaler") { continue }
        $eventTimeText = [string]$event.eventTime
        if (-not $eventTimeText) { $eventTimeText = [string]$event.lastTimestamp }
        if (-not $eventTimeText) { $eventTimeText = [string]$event.metadata.creationTimestamp }
        if (-not $eventTimeText) { continue }
        $eventTime = [DateTime]::Parse($eventTimeText).ToUniversalTime()
        if ($eventTime -ge $NotBefore.AddSeconds(-5)) {
            return $event
        }
    }
    return $null
}

function Get-CurrentTelemetry($Decision) {
    $rps = Get-PrometheusScalar 'sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[1m]))'
    $p95 = Get-PrometheusScalar 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api"}[1m]))) * 1000'
    $sloComplianceRatio = Get-PrometheusScalar 'sum(rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api",le="0.25"}[1m])) / sum(rate(http_request_duration_seconds_count{namespace="optiscale-demo",app="demo-api"}[1m]))'
    $aggregateOccupancy = Get-PrometheusScalar 'sum(rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m]))'
    $hottestOccupancy = Get-PrometheusScalar 'max(sum by(instance) (rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m])))'
    $effectiveServingReplicas = Get-PrometheusScalar 'count(sum by(instance) (rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s])) > 1)'
    $readyReplicas = Get-ReadyTargetReplicas
    $meanServingOccupancy = $null
    if ($null -ne $effectiveServingReplicas -and $effectiveServingReplicas -gt 0 -and $null -ne $aggregateOccupancy) {
        $meanServingOccupancy = $aggregateOccupancy / $effectiveServingReplicas
    }
    return [pscustomobject]@{
        RPS = $rps
        P95Milliseconds = $p95
        SLOComplianceRatio = $sloComplianceRatio
        PredictiveRequestRate = $Decision.predictiveRequestRate
        PredictiveDemandWindowSeconds = $Decision.predictiveDemandWindowSeconds
        DemandObservationLagSeconds = $Decision.demandObservationLagSeconds
        AggregateSlotOccupancy = $aggregateOccupancy
        HottestReplicaOccupancy = $hottestOccupancy
        EffectiveServingReplicas = $effectiveServingReplicas
        ReadyReplicas = $readyReplicas
        RequestedReplicas = $Decision.currentReplicas
        MeanServingOccupancy = $meanServingOccupancy
        PhysicalConcurrencyLimit = $Decision.physicalConcurrencyLimit
        SafeOperatingOccupancy = $Decision.safeOperatingOccupancy
        ForecastConfidence = $Decision.forecastConfidence
        ForecastFitR2 = $Decision.forecastFitR2
        ForecastRPS = $Decision.forecastRequestRate
        ReadinessLeadTimeSeconds = $Decision.readinessLeadTimeSeconds
        ReadinessEvidenceSource = $Decision.readinessEvidenceSource
        ReadinessTemplateIdentity = $Decision.readinessTemplateIdentity
        ControlLoopAllowanceSeconds = $Decision.controlLoopAllowanceSeconds
        PlanningHorizonSeconds = $Decision.forecastHorizonSeconds
        SafePerReplicaCapacity = $Decision.safePerReplicaCapacity
        CurrentSafeCapacity = $Decision.currentSafeCapacity
        PredictionAccepted = $Decision.predictionAccepted
        Action = $Decision.action
        Classification = $Decision.detectedBottleneck
    }
}

function Write-TelemetryLine($Telemetry) {
	Write-Host "utc=$([DateTime]::UtcNow.ToString('HH:mm:ssZ')) rps=$(Format-Value $Telemetry.RPS) p95-ms=$(Format-Value $Telemetry.P95Milliseconds) slo-compliance-250ms=$(Format-Value $Telemetry.SLOComplianceRatio) requested=$($Telemetry.RequestedReplicas) ready=$($Telemetry.ReadyReplicas) effective-serving=$($Telemetry.EffectiveServingReplicas) slot-occupancy-total=$(Format-Value $Telemetry.AggregateSlotOccupancy) hottest-serving-slot-occupancy=$(Format-Value $Telemetry.HottestReplicaOccupancy) mean-slot-occupancy-per-serving=$(Format-Value $Telemetry.MeanServingOccupancy) physical-slot-limit=$(Format-Value $Telemetry.PhysicalConcurrencyLimit) safe-slot-boundary=$(Format-Value $Telemetry.SafeOperatingOccupancy) forecast-confidence=$($Telemetry.ForecastConfidence) r2=$(Format-Value $Telemetry.ForecastFitR2) forecast-rps=$(Format-Value $Telemetry.ForecastRPS) readiness-lead-s=$(Format-Value $Telemetry.ReadinessLeadTimeSeconds) readiness-source=$($Telemetry.ReadinessEvidenceSource) readiness-revision=$($Telemetry.ReadinessTemplateIdentity) control-loop-allowance-s=$(Format-Value $Telemetry.ControlLoopAllowanceSeconds) demand-window-s=$(Format-Value $Telemetry.PredictiveDemandWindowSeconds) demand-observation-lag-s=$(Format-Value $Telemetry.DemandObservationLagSeconds) planning-horizon-s=$(Format-Value $Telemetry.PlanningHorizonSeconds) safe-per-serving-replica=$(Format-Value $Telemetry.SafePerReplicaCapacity) realized-current-safe=$(Format-Value $Telemetry.CurrentSafeCapacity) predictionAccepted=$($Telemetry.PredictionAccepted) classification=$($Telemetry.Classification) action=$($Telemetry.Action) predictive-demand-rps=$(Format-Value $Telemetry.PredictiveRequestRate)"
}

function Validate-HealthyBaseline {
    param($Scaler)

    $sloMilliseconds = [double]$Scaler.spec.slo.targetP95Milliseconds
    $physicalConcurrencyLimit = [double]$Scaler.spec.metric.physicalConcurrencyLimit
    $safeCapacityMargin = [double]$Scaler.spec.prediction.safeCapacityMargin
    $safeOperatingOccupancy = $physicalConcurrencyLimit * $safeCapacityMargin
    $capacityLearningFloor = 0.3 * $safeOperatingOccupancy
    $inventorySpec = $Scaler.spec.dependencies | Where-Object { $_.name -eq "inventory-service" } | Select-Object -First 1
    $databaseSpec = $Scaler.spec.dependencies | Where-Object { $_.name -eq "postgres" } | Select-Object -First 1
    if ($null -eq $inventorySpec -or $null -eq $databaseSpec) {
        throw "The sample OptiScaler must declare inventory-service and postgres before running this topology-specific scenario."
    }
    $inventoryLatencyThreshold = [double]$inventorySpec.thresholds.latencyMilliseconds
    $inventoryWorkThreshold = [double]$inventorySpec.thresholds.utilization
    $databaseLatencyThreshold = [double]$databaseSpec.thresholds.latencyMilliseconds
    $databaseActiveThreshold = [double]$databaseSpec.thresholds.utilization
    $readyReplicas = Get-ReadyTargetReplicas
    $rps = Get-PrometheusScalar 'sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[1m]))'
    $p95 = Get-PrometheusScalar 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api"}[1m]))) * 1000'
    $aggregateOccupancy = Get-PrometheusScalar 'sum(rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m]))'
    $hottestOccupancy = Get-PrometheusScalar 'max(sum by(instance) (rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m])))'
    $effectiveServingReplicas = Get-PrometheusScalar 'count(sum by(instance) (rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s])) > 1)'
    $meanServingOccupancy = $null
    if ($null -ne $effectiveServingReplicas -and $effectiveServingReplicas -gt 0 -and $null -ne $aggregateOccupancy) {
        $meanServingOccupancy = $aggregateOccupancy / $effectiveServingReplicas
    }
    $inventoryP95 = Get-PrometheusScalar 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000'
    $inventoryWork = Get-PrometheusScalar 'sum(avg_over_time(http_active_work{namespace="optiscale-demo",app="inventory-service"}[1m]))'
    $databaseP95 = Get-PrometheusScalar 'histogram_quantile(0.95, sum by (le) (rate(db_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000'
    $databaseActive = Get-PrometheusScalar 'sum(avg_over_time(db_active_requests{namespace="optiscale-demo",app="inventory-service"}[1m]))'

    Write-Host ("Baseline: rps={0} p95={1}ms requested={2} slots-total={3} hottest={4} mean-per-serving={5}; ready={6} effective-serving={7}; inventory-p95={8}ms work={9}; database-p95={10}ms active={11}." -f `
        (Format-Value $rps), (Format-Value $p95), (Format-Value $Scaler.status.currentReplicas), (Format-Value $aggregateOccupancy), (Format-Value $hottestOccupancy), (Format-Value $meanServingOccupancy), $readyReplicas, (Format-Value $effectiveServingReplicas), `
        (Format-Value $inventoryP95), (Format-Value $inventoryWork), (Format-Value $databaseP95), (Format-Value $databaseActive))

    $rpsMinimum = $baselineRate - 20
    $rpsMaximum = $baselineRate + 20
    Write-Host ("INFO: physical concurrency ceiling={0}; safe operating occupancy={1} (= physical limit x {2} margin); capacity-learning floor={3}." -f `
        (Format-Value $physicalConcurrencyLimit), (Format-Value $safeOperatingOccupancy), (Format-Value $safeCapacityMargin), (Format-Value $capacityLearningFloor))

    $failedChecks = [System.Collections.Generic.List[string]]::new()
    $reportCheck = {
        param([bool]$Passed, [string]$PassText, [string]$FailureText)
        if ($Passed) {
            Write-Host "PASS: $PassText"
        }
        else {
            Write-Host "FAIL: $FailureText"
            [void]$failedChecks.Add($FailureText)
        }
    }

    $rpsValid = $null -ne $rps -and $rps -ge $rpsMinimum -and $rps -le $rpsMaximum
    if ($null -eq $rps) {
        $rpsFailure = "demo-api RPS is n/a; required range is $rpsMinimum-$rpsMaximum RPS"
    }
    elseif ($rps -lt $rpsMinimum -or $rps -gt $rpsMaximum) {
        $rpsFailure = "demo-api RPS $(Format-Value $rps) is outside required range $rpsMinimum-$rpsMaximum RPS"
    }
    else {
        $rpsFailure = "demo-api RPS $(Format-Value $rps) is non-finite; required range is $rpsMinimum-$rpsMaximum RPS"
    }
    & $reportCheck $rpsValid "demo-api RPS $(Format-Value $rps) is within required range $rpsMinimum-$rpsMaximum RPS" $rpsFailure

    $readyValid = $readyReplicas -eq 2
    $readyFailure = "demo-api Ready replicas $readyReplicas != required 2"
    & $reportCheck $readyValid "demo-api Ready replicas $readyReplicas == required 2" $readyFailure

    $servingCountValid = $null -ne $effectiveServingReplicas -and $effectiveServingReplicas -ge 0 -and $effectiveServingReplicas -le $readyReplicas -and [Math]::Truncate($effectiveServingReplicas) -eq $effectiveServingReplicas
    if ($null -eq $effectiveServingReplicas) {
        $servingCountFailure = "effective serving replica count is n/a; required integer in [0,$readyReplicas]"
    }
    elseif (-not $servingCountValid) {
        $servingCountFailure = "effective serving replica count $(Format-Value $effectiveServingReplicas) is invalid or exceeds Ready replicas $readyReplicas"
    }
    else {
        $servingCountFailure = "effective serving replica count $(Format-Value $effectiveServingReplicas) is non-finite; required integer in [0,$readyReplicas]"
    }
    & $reportCheck $servingCountValid "effective serving replicas $(Format-Value $effectiveServingReplicas) is a valid count <= Ready replicas $readyReplicas" $servingCountFailure

    $allServingValid = $servingCountValid -and $effectiveServingReplicas -eq $readyReplicas -and $readyReplicas -eq 2
    $allServingFailure = "requested baseline has $readyReplicas Ready but $(Format-Value $effectiveServingReplicas) traffic-bearing replicas; capacity learning requires all 2 baseline replicas to be traffic-bearing"
    & $reportCheck $allServingValid "all $readyReplicas Ready baseline replicas are traffic-bearing" $allServingFailure

    $p95Valid = $null -ne $p95 -and $p95 -lt $sloMilliseconds
    if ($null -eq $p95) {
        $p95Failure = "demo-api p95 is n/a; required < SLO $sloMilliseconds ms"
    }
    elseif ($p95 -ge $sloMilliseconds) {
        $p95Failure = "demo-api p95 $(Format-Value $p95)ms >= SLO $sloMilliseconds ms"
    }
    else {
        $p95Failure = "demo-api p95 $(Format-Value $p95)ms is non-finite; required < SLO $sloMilliseconds ms"
    }
    & $reportCheck $p95Valid "demo-api p95 $(Format-Value $p95)ms < SLO $sloMilliseconds ms" $p95Failure

    $aggregateOccupancyValid = $null -ne $aggregateOccupancy -and $aggregateOccupancy -ge 0
    $aggregateFailure = "aggregate held-slot occupancy is n/a or invalid; required finite nonnegative telemetry"
    & $reportCheck $aggregateOccupancyValid "aggregate held-slot occupancy $(Format-Value $aggregateOccupancy) is finite and nonnegative" $aggregateFailure

    $meanOccupancyAvailable = $null -ne $meanServingOccupancy
    $learningFloorValid = $meanOccupancyAvailable -and $meanServingOccupancy -ge $capacityLearningFloor
    if (-not $meanOccupancyAvailable) {
        $learningFloorFailure = "mean held-slot occupancy per effective serving replica is n/a; required >= learning floor $(Format-Value $capacityLearningFloor)"
    }
    elseif ($meanServingOccupancy -lt $capacityLearningFloor) {
        $learningFloorFailure = "mean held-slot occupancy $(Format-Value $meanServingOccupancy) per serving replica < learning floor $(Format-Value $capacityLearningFloor)"
    }
    else {
        $learningFloorFailure = "mean held-slot occupancy per serving replica is non-finite; required >= learning floor $(Format-Value $capacityLearningFloor)"
    }
    & $reportCheck $learningFloorValid "mean held-slot occupancy $(Format-Value $meanServingOccupancy) per serving replica >= learning floor $(Format-Value $capacityLearningFloor)" $learningFloorFailure

    $meanBelowSafeBoundary = $meanOccupancyAvailable -and $meanServingOccupancy -lt $safeOperatingOccupancy
    if (-not $meanOccupancyAvailable) {
        $meanBoundaryFailure = "mean serving-replica occupancy is n/a; required < safe operating boundary $(Format-Value $safeOperatingOccupancy)"
    }
    elseif ($meanServingOccupancy -ge $safeOperatingOccupancy) {
        $meanBoundaryFailure = "mean held-slot occupancy $(Format-Value $meanServingOccupancy) >= safe operating boundary $(Format-Value $safeOperatingOccupancy)"
    }
    else {
        $meanBoundaryFailure = "mean serving-replica occupancy is non-finite; required < safe operating boundary $(Format-Value $safeOperatingOccupancy)"
    }
    & $reportCheck $meanBelowSafeBoundary "mean held-slot occupancy $(Format-Value $meanServingOccupancy) per serving replica < safe operating boundary $(Format-Value $safeOperatingOccupancy)" $meanBoundaryFailure

    $hottestBelowSafeBoundary = $null -ne $hottestOccupancy -and $hottestOccupancy -ge 0 -and $hottestOccupancy -lt $safeOperatingOccupancy
    if ($null -eq $hottestOccupancy) {
        $hottestFailure = "hottest traffic-bearing replica occupancy is n/a; required < safe boundary $(Format-Value $safeOperatingOccupancy)"
    }
    elseif ($hottestOccupancy -ge $safeOperatingOccupancy) {
        $hottestFailure = "hottest traffic-bearing replica occupancy $(Format-Value $hottestOccupancy) >= safe boundary $(Format-Value $safeOperatingOccupancy)"
    }
    else {
        $hottestFailure = "hottest traffic-bearing replica occupancy is non-finite; required < safe boundary $(Format-Value $safeOperatingOccupancy)"
    }
    & $reportCheck $hottestBelowSafeBoundary "hottest traffic-bearing replica occupancy $(Format-Value $hottestOccupancy) < safe boundary $(Format-Value $safeOperatingOccupancy)" $hottestFailure

    $inventoryP95Valid = $null -ne $inventoryP95 -and $inventoryP95 -lt $inventoryLatencyThreshold
    if ($null -eq $inventoryP95) {
        $inventoryP95Failure = "inventory p95 is n/a; required < threshold $(Format-Value $inventoryLatencyThreshold)ms"
    }
    elseif ($inventoryP95 -ge $inventoryLatencyThreshold) {
        $inventoryP95Failure = "inventory p95 $(Format-Value $inventoryP95)ms >= threshold $(Format-Value $inventoryLatencyThreshold)ms"
    }
    else {
        $inventoryP95Failure = "inventory p95 $(Format-Value $inventoryP95)ms is non-finite; required < threshold $(Format-Value $inventoryLatencyThreshold)ms"
    }
    & $reportCheck $inventoryP95Valid "inventory p95 $(Format-Value $inventoryP95)ms < threshold $(Format-Value $inventoryLatencyThreshold)ms" $inventoryP95Failure

    $inventoryWorkValid = $null -ne $inventoryWork -and $inventoryWork -lt $inventoryWorkThreshold
    if ($null -eq $inventoryWork) {
        $inventoryWorkFailure = "inventory work is n/a; required < threshold $(Format-Value $inventoryWorkThreshold)"
    }
    elseif ($inventoryWork -ge $inventoryWorkThreshold) {
        $inventoryWorkFailure = "inventory work $(Format-Value $inventoryWork) >= threshold $(Format-Value $inventoryWorkThreshold)"
    }
    else {
        $inventoryWorkFailure = "inventory work $(Format-Value $inventoryWork) is non-finite; required < threshold $(Format-Value $inventoryWorkThreshold)"
    }
    & $reportCheck $inventoryWorkValid "inventory work $(Format-Value $inventoryWork) < threshold $(Format-Value $inventoryWorkThreshold)" $inventoryWorkFailure

    $databaseP95Valid = $null -ne $databaseP95 -and $databaseP95 -lt $databaseLatencyThreshold
    if ($null -eq $databaseP95) {
        $databaseP95Failure = "database p95 is n/a; required < threshold $(Format-Value $databaseLatencyThreshold)ms"
    }
    elseif ($databaseP95 -ge $databaseLatencyThreshold) {
        $databaseP95Failure = "database p95 $(Format-Value $databaseP95)ms >= threshold $(Format-Value $databaseLatencyThreshold)ms"
    }
    else {
        $databaseP95Failure = "database p95 $(Format-Value $databaseP95)ms is non-finite; required < threshold $(Format-Value $databaseLatencyThreshold)ms"
    }
    & $reportCheck $databaseP95Valid "database p95 $(Format-Value $databaseP95)ms < threshold $(Format-Value $databaseLatencyThreshold)ms" $databaseP95Failure

    $databaseActiveValid = $null -ne $databaseActive -and $databaseActive -lt $databaseActiveThreshold
    if ($null -eq $databaseActive) {
        $databaseActiveFailure = "database active work is n/a; required < threshold $(Format-Value $databaseActiveThreshold)"
    }
    elseif ($databaseActive -ge $databaseActiveThreshold) {
        $databaseActiveFailure = "database active work $(Format-Value $databaseActive) >= threshold $(Format-Value $databaseActiveThreshold)"
    }
    else {
        $databaseActiveFailure = "database active work $(Format-Value $databaseActive) is non-finite; required < threshold $(Format-Value $databaseActiveThreshold)"
    }
    & $reportCheck $databaseActiveValid "database active work $(Format-Value $databaseActive) < threshold $(Format-Value $databaseActiveThreshold)" $databaseActiveFailure

    if ($failedChecks.Count -gt 0) {
        Stop-K6AndFail ("Baseline validation failed: " + ($failedChecks -join "; "))
    }
    return [pscustomobject]@{
        RPS = $rps
        P95Milliseconds = $p95
        AggregateSlotOccupancy = $aggregateOccupancy
        HottestReplicaOccupancy = $hottestOccupancy
        MeanServingOccupancy = $meanServingOccupancy
        ReadyReplicas = $readyReplicas
        EffectiveServingReplicas = $effectiveServingReplicas
        SLOMilliseconds = $sloMilliseconds
        PhysicalConcurrencyLimit = $physicalConcurrencyLimit
        SafeOperatingOccupancy = $safeOperatingOccupancy
    }
}

# Tear down only this scenario's prior load sources/resources and clear controller memory.
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "scale", "deployment/demo-loadgen", "--replicas=0") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "rollout", "status", "deployment/demo-loadgen", "--timeout=120s") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "delete", "job", $k6JobName, "--ignore-not-found=true", "--wait=true") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "delete", "configmap", $k6ConfigMapName, "--ignore-not-found=true") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $controllerNamespace, "scale", "deployment/optiscaler-controller", "--replicas=0") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $controllerNamespace, "rollout", "status", "deployment/optiscaler-controller", "--timeout=120s") | Out-Host

# Restore the calibrated application/dependency point while OptiScale has no in-memory history.
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "set", "env", "deployment/demo-api", "DEMO_WORK_MS=45", "DEMO_CONCURRENCY=20", "DEMO_READY_DELAY_MS=$readinessDelayMs") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "set", "env", "deployment/inventory-service", "INVENTORY_WORK_MS=10", "INVENTORY_DB_DELAY_MS=0", "INVENTORY_CONCURRENCY=20", "DATABASE_MAX_OPEN_CONNS=20") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "scale", "deployment/demo-api", "--replicas=2") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "scale", "deployment/inventory-service", "--replicas=1") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "scale", "deployment/postgres", "--replicas=1") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "scale", "deployment/demo-loadgen", "--replicas=0") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "rollout", "status", "deployment/demo-api", "--timeout=180s") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "rollout", "status", "deployment/postgres", "--timeout=180s") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "rollout", "status", "deployment/inventory-service", "--timeout=180s") | Out-Host

# Respect the persisted cooldown before the run starts, without collecting stale controller history.
$scaler = Get-OptiScaler
$cooldownSeconds = [int]$scaler.spec.policy.cooldownSeconds
if ($scaler.status.lastScaleTime) {
    $lastScale = [DateTime]::Parse([string]$scaler.status.lastScaleTime).ToUniversalTime()
    $cooldownRemaining = $cooldownSeconds - (([DateTime]::UtcNow - $lastScale).TotalSeconds)
    if ($cooldownRemaining -gt 0) {
        Write-Host "Waiting $([Math]::Ceiling($cooldownRemaining))s for the existing scale cooldown before starting k6."
        Start-Sleep -Seconds ([Math]::Ceiling($cooldownRemaining) + 1)
    }
}

# The checked-in JS is the sole script source; recreate its ConfigMap for deterministic reruns.
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "create", "configmap", $k6ConfigMapName, "--from-file=predictable-ramp.js=$repoK6Script") | Out-Host
$runStartedAt = [DateTime]::UtcNow
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "apply", "-f", $repoK6Job) | Out-Host
$containerStartedAt = Get-K6ContainerStartTime
$baselineEnd = $containerStartedAt.AddSeconds($baselineSeconds)
Write-Host "k6 is holding $baselineRate open arrivals/s. Waiting until the first ${baselineSeconds}s stage has completed to validate the healthy baseline."
while ([DateTime]::UtcNow -lt $baselineEnd) {
    $jobState = Get-K6Job
    if ([int]$jobState.status.failed -gt 0 -or [int]$jobState.status.succeeded -gt 0) {
        Stop-K6AndFail "The k6 Job ended before the baseline validation point."
    }
    $remaining = ($baselineEnd - [DateTime]::UtcNow).TotalSeconds
    Start-Sleep -Seconds ([Math]::Min(5, [Math]::Max(1, [Math]::Ceiling($remaining))))
}
$baseline = Validate-HealthyBaseline (Get-OptiScaler)
Write-Host "Healthy baseline validated at $([Math]::Round($baseline.RPS, 1)) RPS, $([Math]::Round($baseline.P95Milliseconds, 1))ms p95, and $([Math]::Round($baseline.MeanServingOccupancy, 2)) held slots per effective serving replica (hottest $([Math]::Round($baseline.HottestReplicaOccupancy, 2)), boundary $([Math]::Round($baseline.SafeOperatingOccupancy, 2)))."

# The remaining controller-warm-up stage holds the proven 200 RPS point while the cleanly restarted controller learns capacity.
$initialStatus = Get-OptiScaler
$initialScaleSignature = Get-ScaleSignature $initialStatus
Invoke-Kubectl -KubectlArgs @("-n", $controllerNamespace, "scale", "deployment/optiscaler-controller", "--replicas=1") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $controllerNamespace, "rollout", "restart", "deployment/optiscaler-controller") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $controllerNamespace, "rollout", "status", "deployment/optiscaler-controller", "--timeout=120s") | Out-Host
Write-Host "Controller restarted with empty in-memory history. k6 schedule: $baselineRate RPS baseline $baselineSeconds s (validated), $baselineRate RPS controller warm-up $controllerWarmSeconds s, linear $baselineRate->$rampTargetRate RPS over $rampSeconds s, then $rampTargetRate RPS for $postRampSeconds s."

$prescaleObserved = $false
$targetReadyProofObserved = $false
$capacityEvidenceObserved = $false
$preScaleSnapshot = $null
$readyProofSnapshot = $null
$prescaleDecision = $null
$lastSeenScaleSignature = $initialScaleSignature
$rampDeadline = [DateTime]::UtcNow.AddSeconds(390)
while ([DateTime]::UtcNow -lt $rampDeadline) {
    $jobState = Get-K6Job
    if ([int]$jobState.status.failed -gt 0) {
        Show-K6Logs
        throw "k6 Job failed. Its dropped-iteration or HTTP-failure threshold was breached; this run is not a valid arrival-rate proof."
    }

    $current = Get-OptiScaler
    $decision = $current.status.lastDecision
    $telemetry = Get-CurrentTelemetry $decision
    Write-TelemetryLine $telemetry
    if ($decision.predictionRejectedReason) {
        Write-Host "Prediction status: $($decision.predictionRejectedReason)"
    }
    if ($null -ne $decision.safePerReplicaCapacity -and $null -ne $decision.currentSafeCapacity) {
        $capacityEvidenceObserved = $true
    }

    $scaleSignature = Get-ScaleSignature $current
    if ($scaleSignature -ne $lastSeenScaleSignature) {
        $lastSeenScaleSignature = $scaleSignature
        $newScaleDecision = $current.status.lastScaleDecision
        $newScaleAction = [string]$newScaleDecision.action
        if ($newScaleAction -eq "PRESCALE_TARGET" -and -not $prescaleObserved) {
            $liveP95 = Get-PrometheusScalar 'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api"}[1m]))) * 1000'
            if ($null -eq $liveP95 -or $liveP95 -ge $baseline.SLOMilliseconds -or $newScaleDecision.observedTargetP95Milliseconds -ge $baseline.SLOMilliseconds) {
                Stop-K6AndFail "A new PRESCALE_TARGET was recorded only after target p95 reached or exceeded the SLO; the predictive proof failed."
            }
            $telemetry.P95Milliseconds = $liveP95
            $maxErrorRate = [double]$current.spec.prediction.maxErrorRate
            $readinessSourceValid = @("CURRENT_FRESH_SAMPLE", "PERSISTED_LEARNED_SAMPLE") -contains [string]$newScaleDecision.readinessEvidenceSource
            $horizonEvidenceValid = $false
            if ($null -ne $newScaleDecision.readinessLeadTimeSeconds -and $newScaleDecision.readinessLeadTimeSeconds -gt 0 -and `
                $null -ne $newScaleDecision.controlLoopAllowanceSeconds -and $null -ne $newScaleDecision.demandObservationLagSeconds -and `
                $null -ne $newScaleDecision.predictiveDemandWindowSeconds -and $null -ne $newScaleDecision.forecastHorizonSeconds) {
                $demandWindow = [double]$newScaleDecision.predictiveDemandWindowSeconds
                $observationLag = [double]$newScaleDecision.demandObservationLagSeconds
                $expectedPlanningHorizon = [double]$newScaleDecision.readinessLeadTimeSeconds + [double]$newScaleDecision.controlLoopAllowanceSeconds + $observationLag
                $horizonEvidenceValid = [double]$newScaleDecision.controlLoopAllowanceSeconds -eq $reconcileIntervalSeconds -and `
                    [Math]::Abs($observationLag - ($demandWindow / 2)) -lt 0.001 -and `
                    [double]$current.spec.prediction.predictiveDemandWindowSeconds -eq $demandWindow -and `
                    [Math]::Abs([double]$newScaleDecision.forecastHorizonSeconds - $expectedPlanningHorizon) -lt 0.001
            }
            if ($newScaleDecision.forecastConfidence -ne "HIGH" -or $null -eq $newScaleDecision.forecastFitR2 -or $newScaleDecision.forecastFitR2 -lt 0.95 -or `
                -not $horizonEvidenceValid -or `
                $newScaleDecision.detectedBottleneck -ne "HEALTHY" -or $newScaleDecision.predictionAccepted -ne $true -or `
                -not $readinessSourceValid -or `
                $newScaleDecision.chosenTarget -ne "demo-api" -or $null -eq $newScaleDecision.targetErrorRate -or $newScaleDecision.targetErrorRate -gt $maxErrorRate -or `
                $null -eq $newScaleDecision.predictiveRequestRate -or $null -eq $newScaleDecision.forecastRequestRate -or $null -eq $newScaleDecision.currentSafeCapacity -or `
                $newScaleDecision.forecastRequestRate -le $newScaleDecision.currentSafeCapacity -or `
                $newScaleDecision.currentReplicas -ne 2 -or $newScaleDecision.desiredReplicas -ne 3) {
                Stop-K6AndFail "A new PRESCALE_TARGET did not contain the expected healthy, HIGH-quality 2-to-3 capacity proof."
            }
            $deployment = Get-KubectlJson -KubectlArgs @("-n", $namespace, "get", "deployment", "demo-api", "-o", "json")
            if ([int]$deployment.spec.replicas -ne 3) {
                Stop-K6AndFail "PRESCALE_TARGET was recorded but the demo-api Deployment does not show the real /scale target of 3 replicas."
            }
            $prescaleObserved = $true
            $prescaleDecision = $newScaleDecision
            $preScaleSnapshot = [ordered]@{
                recordedAtUtc = [DateTime]::UtcNow.ToString("o")
                k6RunStartedAtUtc = $runStartedAt.ToString("o")
                scaleSignature = $scaleSignature
                prescaleDecisionTimestamp = $newScaleDecision.timestamp
                measuredReadinessLeadTimeSeconds = $newScaleDecision.readinessLeadTimeSeconds
                readinessEvidenceSource = $newScaleDecision.readinessEvidenceSource
                readinessTemplateIdentity = $newScaleDecision.readinessTemplateIdentity
                controlLoopAllowanceSeconds = $newScaleDecision.controlLoopAllowanceSeconds
                predictiveDemandRate = $newScaleDecision.predictiveRequestRate
                predictiveDemandWindowSeconds = $newScaleDecision.predictiveDemandWindowSeconds
                demandObservationLagSeconds = $newScaleDecision.demandObservationLagSeconds
                planningHorizonSeconds = $newScaleDecision.forecastHorizonSeconds
                decision = $newScaleDecision
                latestTelemetry = $telemetry
                liveP95Milliseconds = $liveP95
                sloMilliseconds = $baseline.SLOMilliseconds
                targetDeploymentReplicas = $deployment.spec.replicas
            }
            Write-Host ("PRESCALE_PROOF_STATE=" + ($preScaleSnapshot | ConvertTo-Json -Compress -Depth 12))
            Write-Host "New PRESCALE_TARGET verified before the SLO breach; continuing the k6 plateau to observe post-scale behavior."
        }
        elseif ($newScaleAction -eq "SCALE_TARGET" -and -not $prescaleObserved) {
            Stop-K6AndFail "A new reactive SCALE_TARGET occurred before PRESCALE_TARGET; stopping the ramp proof."
        }
        elseif (-not $prescaleObserved) {
            Stop-K6AndFail "An unexpected new scale action ($newScaleAction) occurred before PRESCALE_TARGET."
        }
        elseif ($newScaleAction -ne "PRESCALE_TARGET") {
            Write-Warning "A later scale action occurred after the prescale proof: $newScaleAction. The script will finish the post-prescale observation window."
        }
    }

    if ($prescaleObserved -and -not $targetReadyProofObserved) {
        if ($null -ne $telemetry.P95Milliseconds -and $telemetry.P95Milliseconds -ge $baseline.SLOMilliseconds) {
            Stop-K6AndFail "Target p95 reached or exceeded the SLO before the third demo-api replica became Ready; stopping the predictive readiness proof."
        }
        if ($telemetry.ReadyReplicas -eq 3) {
            if ($null -eq $telemetry.P95Milliseconds) {
                Stop-K6AndFail "The third demo-api replica became Ready, but target p95 telemetry is unavailable; the SLO-safe readiness proof cannot be established."
            }
            $targetReadyProofObserved = $true
            $readyProofSnapshot = [ordered]@{
                provenAtUtc = [DateTime]::UtcNow.ToString("o")
                prescaleDecisionId = $prescaleDecision.id
                prescaleDecisionTimestamp = $prescaleDecision.timestamp
                measuredReadinessLeadTimeSeconds = $prescaleDecision.readinessLeadTimeSeconds
                readinessEvidenceSource = $prescaleDecision.readinessEvidenceSource
                readinessTemplateIdentity = $prescaleDecision.readinessTemplateIdentity
                controlLoopAllowanceSeconds = $prescaleDecision.controlLoopAllowanceSeconds
                predictiveDemandRate = $prescaleDecision.predictiveRequestRate
                predictiveDemandWindowSeconds = $prescaleDecision.predictiveDemandWindowSeconds
                demandObservationLagSeconds = $prescaleDecision.demandObservationLagSeconds
                planningHorizonSeconds = $prescaleDecision.forecastHorizonSeconds
                readyReplicas = $telemetry.ReadyReplicas
                currentP95Milliseconds = $telemetry.P95Milliseconds
                sloMilliseconds = $baseline.SLOMilliseconds
            }
            Write-Host ("PRESCALE_READY_PROOF=" + ($readyProofSnapshot | ConvertTo-Json -Compress -Depth 8))
            Write-Host "The third demo-api replica became Ready while target p95 remained below the SLO."
        }
        elseif ($null -eq $telemetry.P95Milliseconds) {
            Stop-K6AndFail "Target p95 telemetry became unavailable after PRESCALE_TARGET and before the third replica became Ready; the SLO-safe readiness proof cannot be established."
        }
    }

    if (-not $prescaleObserved -and $null -ne $telemetry.P95Milliseconds -and $telemetry.P95Milliseconds -ge $baseline.SLOMilliseconds) {
        Stop-K6AndFail "Target p95 reached or exceeded the SLO before PRESCALE_TARGET; stopping the ramp proof."
    }

    if ([int]$jobState.status.succeeded -gt 0 -and (-not $prescaleObserved -or $targetReadyProofObserved)) {
        break
    }
    Start-Sleep -Seconds $pollSeconds
}

$finalJob = Get-K6Job
if ([int]$finalJob.status.succeeded -eq 0) {
    Stop-K6AndFail "The k6 ramp did not complete within its observation deadline."
}
Show-K6Logs
if (-not $capacityEvidenceObserved) {
    throw "The controller never exposed safe capacity evidence; the required healthy capacity-learning gate was not confirmed."
}
if (-not $prescaleObserved) {
	throw "No new PRESCALE_TARGET occurred during this run; persisted historical lastScaleDecision was not treated as a new result."
}
if (-not $targetReadyProofObserved) {
	throw "The third demo-api replica did not become Ready with p95 below the SLO during this run."
}

$preScaleEvent = $null
$eventDeadline = [DateTime]::UtcNow.AddSeconds(20)
while ($null -eq $preScaleEvent -and [DateTime]::UtcNow -lt $eventDeadline) {
    $preScaleEvent = Get-PreScaleEvent $runStartedAt
    if ($null -eq $preScaleEvent) { Start-Sleep -Seconds 2 }
}
if ($null -eq $preScaleEvent) {
    throw "PRESCALE_TARGET was observed, but no PreScaleTarget Kubernetes Event from this run was found."
}
Write-Host ("PRESCALE_EVENT=" + ($preScaleEvent | ConvertTo-Json -Compress -Depth 8))

Write-Host "Final OptiScaler decision and workload state:"
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "get", "optiscaler", "demo-api", "-o", "yaml") | Out-Host
Invoke-Kubectl -KubectlArgs @("-n", $namespace, "get", "deployments", "demo-api", "inventory-service", "postgres", "demo-loadgen") | Out-Host
Write-Host "Predictable-ramp proof completed: a new PRESCALE_TARGET scaled demo-api 2->3 and all three replicas became Ready before p95 reached the SLO. The k6 Job result and event are retained for inspection; the old curl loadgen remains scaled to zero."
