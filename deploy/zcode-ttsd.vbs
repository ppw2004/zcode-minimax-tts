' Kept in the user's Startup folder: at logon, make sure the ZCode TTS daemon
' is running (pings it; starts it via the ZCodeTTSWatcher scheduled task only
' if dead). Window style 0 = no console flash.
'
' Deploy to: %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\zcode-ttsd.vbs
CreateObject("WScript.Shell").Run "powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Users\<you>\.zcode\hooks\ttsd-ensure.ps1", 0, False
