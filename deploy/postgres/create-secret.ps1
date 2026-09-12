param(
    [string]$Namespace = "optiscale-demo",
    [string]$Username = $(if ($env:OPTISCALE_POSTGRES_USER) { $env:OPTISCALE_POSTGRES_USER } else { "optiscale" }),
    [string]$Password = $(if ($env:OPTISCALE_POSTGRES_PASSWORD) { $env:OPTISCALE_POSTGRES_PASSWORD } else { "optiscale-local-only-change-me" })
)

$ErrorActionPreference = "Stop"
if ($Password -eq "optiscale-local-only-change-me") {
    Write-Warning "Using the documented local-only development password. Do not reuse it outside Minikube."
}

kubectl create secret generic optiscale-postgres `
    --namespace $Namespace `
    --from-literal="username=$Username" `
    --from-literal="password=$Password" `
    --dry-run=client -o yaml | kubectl apply -f -
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create/update the local OptiScale PostgreSQL Secret."
}
