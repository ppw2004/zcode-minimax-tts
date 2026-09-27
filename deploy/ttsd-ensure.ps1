# ZCode hook (SessionStart / UserPromptSubmit): keep the TTS daemon alive.
# Pings the daemon; if it does not answer, starts it via the scheduled task so
# the daemon escapes this hook's job object. Exits fast either way.
#
# Deploy to: %USERPROFILE%\.zcode\hooks\ttsd-ensure.ps1
$ErrorActionPreference = 'SilentlyContinue'
try {
    $r = Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:9750/ping' -TimeoutSec 1
    if ($r.StatusCode -eq 200) { exit 0 }
} catch {}
schtasks /run /tn 'ZCodeTTSWatcher' | Out-Null
exit 0
