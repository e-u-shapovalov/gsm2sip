#!/bin/bash
#
# Build script for SIP-GSM Gateway APK + Magisk module.
#
# Prerequisites:
#   sudo ./setup.sh    # run once to install JDK, Android SDK, etc.
#
# Usage:
#   ./build.sh          # Build debug APK + Magisk module
#   ./build.sh release  # Build release APK + Magisk module
#
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# ── Source environment if available ──────────────────

if [ -f "$SCRIPT_DIR/.env.build" ]; then
    source "$SCRIPT_DIR/.env.build"
fi

# ── Check prerequisites ──────────────────────────────

check_java() {
    if ! command -v java &>/dev/null; then
        echo "ERROR: Java not found. Run: sudo ./setup.sh"
        exit 1
    fi
    JAVA_VER=$(java -version 2>&1 | head -1 | cut -d'"' -f2 | cut -d'.' -f1)
    if [ "$JAVA_VER" -lt 17 ] 2>/dev/null; then
        echo "ERROR: JDK 17+ required (found: $JAVA_VER). Run: sudo ./setup.sh"
        exit 1
    fi
    echo "Java: $(java -version 2>&1 | head -1)"
}

check_android_sdk() {
    # Find Android SDK
    if [ -z "$ANDROID_HOME" ] && [ -z "$ANDROID_SDK_ROOT" ]; then
        for dir in "/opt/android-sdk" "$HOME/Android/Sdk" "$HOME/android-sdk"; do
            if [ -d "$dir" ]; then
                export ANDROID_HOME="$dir"
                break
            fi
        done
    fi
    ANDROID_HOME="${ANDROID_HOME:-$ANDROID_SDK_ROOT}"

    if [ -z "$ANDROID_HOME" ] || [ ! -d "$ANDROID_HOME" ]; then
        echo "ERROR: Android SDK not found. Run: sudo ./setup.sh"
        exit 1
    fi

    # Ensure local.properties exists
    echo "sdk.dir=$ANDROID_HOME" > local.properties
    echo "Android SDK: $ANDROID_HOME"
}

check_gradle_wrapper() {
    local JAR="$SCRIPT_DIR/gradle/wrapper/gradle-wrapper.jar"
    local NEED_DOWNLOAD=false

    if [ ! -f "$JAR" ]; then
        NEED_DOWNLOAD=true
    elif ! unzip -t "$JAR" >/dev/null 2>&1; then
        echo "WARNING: gradle-wrapper.jar is corrupt, re-downloading..."
        rm -f "$JAR"
        NEED_DOWNLOAD=true
    fi

    if [ "$NEED_DOWNLOAD" = true ]; then
        echo "Downloading Gradle wrapper..."
        mkdir -p "$SCRIPT_DIR/gradle/wrapper"

        local GRADLE_VER="8.5"
        local GRADLE_URL="https://services.gradle.org/distributions/gradle-${GRADLE_VER}-bin.zip"

        curl -fsSL "$GRADLE_URL" -o /tmp/gradle-dist.zip
        rm -rf /tmp/gradle-extract
        mkdir -p /tmp/gradle-extract
        unzip -qo /tmp/gradle-dist.zip -d /tmp/gradle-extract

        local GRADLE_BIN="/tmp/gradle-extract/gradle-${GRADLE_VER}/bin/gradle"
        if [ -x "$GRADLE_BIN" ]; then
            cd "$SCRIPT_DIR"
            "$GRADLE_BIN" wrapper --gradle-version "$GRADLE_VER"
        fi

        rm -f /tmp/gradle-dist.zip
        rm -rf /tmp/gradle-extract
    fi

    if [ ! -f "$JAR" ] || ! unzip -t "$JAR" >/dev/null 2>&1; then
        echo "ERROR: Valid gradle-wrapper.jar not found. Run: sudo ./setup.sh"
        exit 1
    fi

    chmod +x "$SCRIPT_DIR/gradlew" 2>/dev/null || true
}

# ── Build APK ────────────────────────────────────────

build_apk() {
    local BUILD_TYPE="${1:-debug}"
    echo ""
    echo "=== Building $BUILD_TYPE APK ==="
    echo ""

    if [ "$BUILD_TYPE" = "release" ]; then
        ./gradlew assembleRelease --no-daemon
        # Signed via the "shared" signingConfig, so the output is app-release.apk
        # rather than app-release-unsigned.apk.  An unsigned APK is rejected by
        # PackageManager with INSTALL_PARSE_FAILED_NO_CERTIFICATES and is
        # useless in the priv-app module, which is what the old path produced.
        APK_PATH="app/build/outputs/apk/release/app-release.apk"
        [ -f "$APK_PATH" ] || APK_PATH="app/build/outputs/apk/release/app-release-unsigned.apk"
    else
        ./gradlew assembleDebug --no-daemon
        APK_PATH="app/build/outputs/apk/debug/app-debug.apk"
    fi

    if [ -f "$APK_PATH" ]; then
        echo ""
        echo "APK built: $APK_PATH"
        cp "$APK_PATH" "$SCRIPT_DIR/gateway.apk"
        echo "Copied to: $SCRIPT_DIR/gateway.apk"
    else
        echo "ERROR: APK not found at $APK_PATH"
        exit 1
    fi
}

# ── Build Magisk module ─────────────────────────────

# Build one tinymix for one ABI.  $1 = GOARCH, $2 = expected `file` signature,
# $3 = output path.
build_tinymix_arch() {
    local GOARCH_="$1" SIG="$2" OUT="$3"
    local TINYMIX_SRC="$SCRIPT_DIR/tools/tinymix"

    # From source whenever Go is there: a binary already in magisk/ may be
    # older than tools/tinymix, and the module would ship it silently.
    if command -v go &>/dev/null && [ -f "$TINYMIX_SRC/main.go" ]; then
        echo "Building tinymix for $GOARCH_..."
        if (cd "$TINYMIX_SRC" && GOOS=linux GOARCH="$GOARCH_" GOARM=7 CGO_ENABLED=0 \
            go build -trimpath -ldflags='-s -w' -o "$OUT" .); then
            chmod 755 "$OUT"
            echo "tinymix built: $OUT ($(du -h "$OUT" | cut -f1))"
            return 0
        fi
    fi

    # No Go: a pre-built binary of the right architecture is the fallback
    if [ -f "$OUT" ] && file "$OUT" 2>/dev/null | grep -q "$SIG"; then
        echo "WARNING: using pre-built tinymix, not rebuilt from source: $OUT"
        return 0
    fi

    echo "WARNING: could not build tinymix for $GOARCH_ — mixer controls will"
    echo "         not work on $GOARCH_ devices.  Install Go, or drop a"
    echo "         pre-built binary at: $OUT"
    return 1
}

build_tinymix() {
    echo ""
    echo "=== Building tinymix (static, one per ABI) ==="
    echo ""

    # Two builds, because the ALSA control ioctls encode the size of structs
    # that contain `long`: an arm64 binary talks a different ioctl ABI than an
    # armeabi-v7a one, and neither works on the other's kernel.  install.sh
    # picks the matching one at flash time.
    build_tinymix_arch arm64 "ARM aarch64" "$SCRIPT_DIR/magisk/tinymix"
    build_tinymix_arch arm   "ELF 32-bit.*ARM" "$SCRIPT_DIR/magisk/tinymix32"
}

# mi6bridge: raw ALSA in-call audio helper for MSM8998 (Xiaomi Mi 6), arm64.
build_mi6bridge() {
    local SRC="$SCRIPT_DIR/tools/mi6bridge" OUT="$SCRIPT_DIR/magisk/mi6bridge"
    echo ""
    echo "=== Building mi6bridge (arm64) ==="
    echo ""
    if command -v go &>/dev/null && [ -f "$SRC/main.go" ]; then
        if (cd "$SRC" && go test ./... && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
            go build -trimpath -ldflags='-s -w' -o "$OUT" .); then
            chmod 755 "$OUT"
            echo "mi6bridge built: $OUT ($(du -h "$OUT" | cut -f1))"
            return 0
        fi
        echo "ERROR: mi6bridge build failed"
        exit 1
    fi
    if [ -f "$OUT" ]; then
        echo "WARNING: using pre-built mi6bridge, not rebuilt from source: $OUT"
        return 0
    fi
    echo "WARNING: no Go and no pre-built mi6bridge — MSM8998 audio will not work"
}

build_magisk() {
    echo ""
    echo "=== Building Magisk module ==="
    echo ""

    # Build tinymix binary for ABOX mixer control (required on Samsung Exynos)
    build_tinymix
    build_mi6bridge

    # Copy the APK into the Magisk module as a system priv-app.
    # This makes the app a privileged system app, enabling permissions
    # like CAPTURE_AUDIO_OUTPUT that are required for telephony audio capture.
    mkdir -p "$SCRIPT_DIR/magisk/system/priv-app/Gateway"
    cp "$SCRIPT_DIR/gateway.apk" "$SCRIPT_DIR/magisk/system/priv-app/Gateway/Gateway.apk"
    echo "Included APK as priv-app in Magisk module"

    cd "$SCRIPT_DIR/magisk"
    rm -f "$SCRIPT_DIR/gateway-magisk.zip"
    zip -r "$SCRIPT_DIR/gateway-magisk.zip" . \
        -x "*.DS_Store" -x "__MACOSX/*"
    echo "Magisk module: $SCRIPT_DIR/gateway-magisk.zip"
    cd "$SCRIPT_DIR"
}

# ── Install to device (if connected via ADB) ────────

install_to_device() {
    # Installing to "whatever is plugged in" is the wrong default once there is
    # more than one phone on the bus — the gateway has been installed onto the
    # wrong handset this way.  SKIP_INSTALL=1 builds the artifacts and stops.
    if [ -n "${SKIP_INSTALL:-}" ]; then
        echo ""
        echo "SKIP_INSTALL set — built artifacts only, nothing installed."
        return 0
    fi

    local COUNT
    COUNT=$(adb devices 2>/dev/null | grep -c "device$")
    if [ "${COUNT:-0}" -gt 1 ]; then
        echo ""
        echo "$COUNT devices connected — refusing to guess which one to install to."
        echo "Install explicitly:  adb -s <serial> install -r gateway.apk"
        return 0
    fi

    if command -v adb &>/dev/null && adb devices 2>/dev/null | grep -q "device$"; then
        echo ""
        echo "=== Device detected — installing ==="
        adb install -r "$SCRIPT_DIR/gateway.apk"
        echo "APK installed."
        echo ""
        echo "To install Magisk module:"
        echo "  adb push gateway-magisk.zip /sdcard/"
        echo "  Then install via Magisk Manager on the device."
    else
        echo ""
        echo "No ADB device connected. To install manually:"
        echo "  adb install gateway.apk"
        echo "  adb push gateway-magisk.zip /sdcard/"
    fi
}

# ── Main ─────────────────────────────────────────────

echo "=== SIP-GSM Gateway Build ==="
echo ""

check_java
check_android_sdk
check_gradle_wrapper

BUILD_TYPE="${1:-debug}"
build_apk "$BUILD_TYPE"
build_magisk
install_to_device

echo ""
echo "=== Build complete ==="
echo "  APK:    $SCRIPT_DIR/gateway.apk"
echo "  Magisk: $SCRIPT_DIR/gateway-magisk.zip"
echo ""
echo "Deploy to device:"
echo "  1. adb push gateway-magisk.zip /sdcard/"
echo "     Install via Magisk Manager -> Modules, then reboot"
echo "     (APK is included in the module as a priv-app)"
echo "  2. After reboot: open app, grant permissions, set as default phone app"
echo "  3. Enter SIP credentials, tap START"
echo ""
echo "NOTE: Do NOT also 'adb install' — the Magisk module installs the APK"
echo "      as a privileged system app with CAPTURE_AUDIO_OUTPUT permission."
