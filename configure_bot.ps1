param(
    [string]$WebAppUrl = "https://sendandsafe.147-45-224-216.sslip.io"
)

$ErrorActionPreference = "Stop"

$secureToken = Read-Host "Bot token from @BotFather" -AsSecureString
$pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureToken)
try {
    $token = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer)
}
finally {
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer)
}

if ([string]::IsNullOrWhiteSpace($token)) {
    throw "Bot token is required."
}

$api = "https://api.telegram.org/bot$token"
$menuButton = @{
    menu_button = @{
        type = "web_app"
        text = "Open Send and Safe"
        web_app = @{
            url = $WebAppUrl
        }
    }
} | ConvertTo-Json -Depth 5

$description = @{
    description = "Private encrypted file transfer. Files are encrypted on your device and deleted after 48 hours."
} | ConvertTo-Json

$shortDescription = @{
    short_description = "Encrypted file transfer with automatic deletion."
} | ConvertTo-Json

$me = Invoke-RestMethod -Method Post -Uri "$api/getMe"
if (-not $me.ok) {
    throw "Telegram rejected the bot token."
}
if ($me.result.username -ne "sendandsafe_bot") {
    throw "This token belongs to @$($me.result.username), not @sendandsafe_bot."
}

foreach ($request in @(
    @{ Method = "setChatMenuButton"; Body = $menuButton },
    @{ Method = "setMyDescription"; Body = $description },
    @{ Method = "setMyShortDescription"; Body = $shortDescription }
)) {
    $response = Invoke-RestMethod `
        -Method Post `
        -Uri "$api/$($request.Method)" `
        -ContentType "application/json" `
        -Body $request.Body
    if (-not $response.ok) {
        throw "Telegram method $($request.Method) failed."
    }
}

Write-Host "Configured @sendandsafe_bot"
Write-Host "Mini App URL: $WebAppUrl"
