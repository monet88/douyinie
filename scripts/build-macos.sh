#!/usr/bin/env bash
set -euo pipefail
export WSLENV="GOOS/u:GOARCH/u:${WSLENV:-}"
export GOOS=darwin
export GOARCH=arm64

echo "========================================================"
echo " Douyinie macOS (Apple Silicon ARM64) Build Script"
echo "========================================================"
GO_BIN="go"
if ! command -v go &>/dev/null; then
    for cand in "/mnt/c/Program Files/Go/bin" "/c/Program Files/Go/bin" "/usr/local/go/bin"; do
        if [ -d "$cand" ]; then
            export PATH="$cand:$PATH"
            break
        fi
    done
fi
if ! command -v go &>/dev/null && command -v go.exe &>/dev/null; then
    GO_BIN="go.exe"
fi

CMD_EXE=""
if command -v cmd.exe &>/dev/null; then
    CMD_EXE="cmd.exe"
elif [ -x "/mnt/c/Windows/System32/cmd.exe" ]; then
    CMD_EXE="/mnt/c/Windows/System32/cmd.exe"
fi
APP_NAME="Douyinie"
DIST_DIR="dist/${APP_NAME}.app"
CONTENTS_DIR="${DIST_DIR}/Contents"
MACOS_DIR="${CONTENTS_DIR}/MacOS"
RESOURCES_DIR="${CONTENTS_DIR}/Resources"

echo "[1/5] Cleaning and creating .app bundle directory..."
rm -rf "${DIST_DIR}"
mkdir -p "${MACOS_DIR}"
mkdir -p "${RESOURCES_DIR}/adapters"
mkdir -p "${RESOURCES_DIR}/models"
mkdir -p "${RESOURCES_DIR}/bin"
mkdir -p "dist_installer"

echo "[2/5] Compiling Douyinie Desktop binary for darwin/arm64..."
if [ -n "$CMD_EXE" ]; then
    "$CMD_EXE" /c "set GOOS=darwin&& set GOARCH=arm64&& go build -o ${MACOS_DIR}\douyinie ./cmd/desktop"
    echo "[3/5] Compiling StageWorker binary for darwin/arm64..."
    "$CMD_EXE" /c "set GOOS=darwin&& set GOARCH=arm64&& go build -o ${RESOURCES_DIR}\stageworker ./cmd/stageworker"
else
    GOOS=darwin GOARCH=arm64 $GO_BIN build -ldflags="-s -w" -o "${MACOS_DIR}/douyinie" ./cmd/desktop
    echo "[3/5] Compiling StageWorker binary for darwin/arm64..."
    GOOS=darwin GOARCH=arm64 $GO_BIN build -ldflags="-s -w" -o "${RESOURCES_DIR}/stageworker" ./cmd/stageworker
fi

echo "[4/5] Copying bundle metadata and adapters..."
cp installer/macos/Info.plist "${CONTENTS_DIR}/Info.plist"
echo -n "APPL????" > "${CONTENTS_DIR}/PkgInfo"
cp cmd/stageworker/adapters/*.py "${RESOURCES_DIR}/adapters/" 2>/dev/null || true

chmod +x "${MACOS_DIR}/douyinie" "${RESOURCES_DIR}/stageworker"

echo "[5/5] Packaging bundle..."
if [[ "$(uname -s)" == "Darwin" ]]; then
    echo "Running native macOS packaging (hdiutil)..."
    xattr -cr "${DIST_DIR}" || true
    hdiutil create -volname "${APP_NAME}" -srcfolder "${DIST_DIR}" -ov -format UDZO "dist_installer/${APP_NAME}-macOS-arm64.dmg"
    echo "[SUCCESS] macOS DMG created: dist_installer/${APP_NAME}-macOS-arm64.dmg"
else
    echo "Cross-compiling environment detected: compressing .app bundle into .zip archive..."
    (cd dist && tar -czf "../dist_installer/${APP_NAME}-macOS-arm64.tar.gz" "${APP_NAME}.app")
    echo "[SUCCESS] macOS archive created: dist_installer/${APP_NAME}-macOS-arm64.tar.gz"
fi

echo "========================================================"
echo " Build complete!"
echo "========================================================"
