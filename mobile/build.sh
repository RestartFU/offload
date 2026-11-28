#!/bin/sh
# Builds the Android app with no SDK installed: JDK 21 and the Android SDK are fetched once into
# .toolchain/ (ignored by git, so it stays warm in an offload workspace). Pass Gradle tasks, e.g.
#   offload run --platform linux/amd64 -- mobile/build.sh :app:testDebugUnitTest
#   offload run --platform linux/amd64 --get mobile/app/build/outputs/apk -- mobile/build.sh :app:assembleDebug
set -eu
cd "$(dirname "$0")"
if [ "$(uname -s)-$(uname -m)" != "Linux-x86_64" ]; then
  echo "mobile/build.sh: Android's build tools (aapt2) come for x86-64 Linux only; this is $(uname -s) $(uname -m)." >&2
  echo "Run it on such a runner: offload run --platform linux/amd64 -- mobile/build.sh ..." >&2
  exit 1
fi
T="$PWD/.toolchain"
mkdir -p "$T/home"
if ! "$T/jdk/bin/java" -version >/dev/null 2>&1; then
  curl -fsSL -o "$T/jdk.tgz" "https://api.adoptium.net/v3/binary/latest/21/ga/linux/x64/jdk/hotspot/normal/eclipse"
  rm -rf "$T/jdk" && mkdir "$T/jdk" && tar -xzf "$T/jdk.tgz" -C "$T/jdk" --strip-components=1 && rm "$T/jdk.tgz"
fi
export JAVA_HOME="$T/jdk" PATH="$T/jdk/bin:$PATH" ANDROID_HOME="$T/sdk" GRADLE_USER_HOME="$T/gradle" OFFLOAD_MOBILE_HOME="$T/home" ANDROID_USER_HOME="$T/android"
if [ ! -f "$ANDROID_HOME/platforms/android-35/android.jar" ] || [ ! -x "$ANDROID_HOME/build-tools/34.0.0/aapt2" ]; then
  curl -fsSL -o "$T/clt.zip" https://dl.google.com/android/repository/commandlinetools-linux-15859902_latest.zip
  echo "4e4c464f145a7512b57d088ac6c278c03c9eea610886b35a5e0804e74eedf583  $T/clt.zip" | sha256sum -c -
  rm -rf "$T/clt" "$ANDROID_HOME/cmdline-tools/latest" && mkdir -p "$ANDROID_HOME/cmdline-tools"
  unzip -q "$T/clt.zip" -d "$T/clt" && mv "$T/clt/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest" && rm -rf "$T/clt" "$T/clt.zip"
  yes | "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" --licenses >/dev/null || true
  "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" "platforms;android-35" "build-tools;34.0.0" >/dev/null
fi
exec ./gradlew --no-daemon "$@"
