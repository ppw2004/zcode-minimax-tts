# Auto-speak hook for Claude Code / ZCode on Windows
# Reads the Stop hook JSON from stdin and speaks the first sentence of the
# response using the speak-text binary (OpenAI or MiniMax TTS).

$ErrorActionPreference = 'SilentlyContinue'

$pluginRoot = if ($env:CLAUDE_PLUGIN_ROOT) { $env:CLAUDE_PLUGIN_ROOT } else { Join-Path $env:USERPROFILE '.claude\plugins\claude-code-tts' }
$speakBin = Join-Path $pluginRoot 'bin\speak-text.exe'
if (-not (Test-Path $speakBin)) { $speakBin = Join-Path $pluginRoot 'bin\speak-text' }
if (-not (Test-Path $speakBin)) { exit 0 }

$raw = [Console]::In.ReadToEnd()
if (-not $raw) { exit 0 }

$msg = $null
try {
    $j = $raw | ConvertFrom-Json
    $msg = $j.stop_hook_message
    if (-not $msg) { $msg = $j.message }
    if (-not $msg -and $j.content) { $msg = $j.content }
} catch {}

if (-not $msg -or $msg.Length -lt 30) { exit 0 }

# First sentence (up to first period), capped at 200 chars
$summary = ($msg -split '\.')[0] + '.'
if ($summary.Length -gt 200) { $summary = $summary.Substring(0, 200) }

# Launch detached so the hook returns immediately; strip quotes from the text
# to keep argument quoting simple.
$summary = $summary.Replace('"', '')
Start-Process -WindowStyle Hidden -FilePath $speakBin -ArgumentList ('"' + $summary + '"')
exit 0
