# THROWAWAY PROTOTYPE SERVER
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $here
if (Get-Command py -ErrorAction SilentlyContinue) {
  py -m http.server 4173 --bind 127.0.0.1
} elseif (Get-Command python -ErrorAction SilentlyContinue) {
  python -m http.server 4173 --bind 127.0.0.1
} else {
  throw 'Python launcher not found. Open index.html directly instead.'
}
