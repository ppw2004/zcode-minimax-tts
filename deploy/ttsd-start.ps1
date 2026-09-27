# Target of the "ZCodeTTSWatcher" scheduled task. Runs the TTS daemon as a
# fully detached process: the exe is built GUI-subsystem (no console) and
# Start-Process returns immediately, so this powershell — and the task
# instance — exits within a second. Nothing stays attached to the task, so
# its 72h "stop task" limit can never kill the daemon, and there is no
# console window on the desktop to close by accident.
#
# Deploy to: %USERPROFILE%\.zcode\hooks\ttsd-start.ps1
$env:TTS_PROVIDER = 'minimax'
$env:MINIMAX_API_KEY = '<YOUR_MINIMAX_KEY>'
$env:TTS_PORT = '9750'
$env:TTS_GAP_MS = '500'
Start-Process -WindowStyle Hidden -FilePath "$PSScriptRoot\ttsd.exe"
