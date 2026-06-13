$ErrorActionPreference = "Stop"

Push-Location (Join-Path $PSScriptRoot "web")
try {
    npm run build
}
finally {
    Pop-Location
}

go run ./cmd/server
