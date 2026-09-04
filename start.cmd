@echo off
rem olcvpn — запуск с правами администратора (нужны для режима TUN).
cd /d "%~dp0"
powershell -NoProfile -Command "Start-Process -FilePath '%~dp0olcvpn.exe' -WorkingDirectory '%~dp0' -Verb RunAs"
