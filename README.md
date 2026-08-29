# Kindle as a Pixel display

[![CI](https://github.com/barakplasma/kindleD/actions/workflows/ci.yml/badge.svg)](https://github.com/barakplasma/kindleD/actions/workflows/ci.yml)

Use a jailbroken 2024 Kindle as a low-power e-ink display and scroll
controller for a Pixel 10, over the Pixel's own Wi-Fi hotspot, with no
router, no internet and no hotel Wi-Fi involved.

```
Pixel 10                             Kindle 2024
├── Android app                      └── kindled
│   ├── virtual display                  ├── connect to the hotspot gateway
│   ├── capture + JPEG encode            ├── receive JPEG
│   ├── TCP server :45831   ◄── Wi-Fi ──►├── paint it with eips
│   └── accessibility service            └── report taps and scrolls
└── Wi-Fi hotspot
```

The architectural boundary is the whole design:

* **Kindle** = network + eips + touchscreen. Nothing else.
* **Pixel** = everything complicated.

The wire between them is [PROTOCOL.md](PROTOCOL.md).

## Repository layout

```
android/    the Pixel app (Kotlin, no dependencies, no AndroidX)
kindle/     kindled, the Kindle daemon (Go, no dependencies, static binary)
kindle/cmd/fakepixel/   a stand-in server for testing without a phone
scripts/    build, deploy and on-device launcher
PROTOCOL.md the wire protocol and why it looks like that
```

## Design decisions worth knowing before you read the code

**Never queue frames.** If frames 101, 102 and 103 are produced while the
Kindle is still painting 101, then 102 is dropped and 103 is painted next.
Latency matters far more than completeness at 3 FPS on e-ink. Both ends
enforce this independently: the Pixel keeps one frame slot and waits for an
ACK (or a 1 s deadline), and the Kindle keeps one frame slot in front of the
panel.

**The Kindle dials out.** It reads the default gateway from
`/proc/net/route`, which on a phone hotspot is the phone. Nothing has to
discover the Kindle, and nothing cares what address DHCP gave it.

**Losing the link is normal, not exceptional.** The Kindle reconnects with
capped exponential backoff, forever. The Pixel accepts a reconnecting Kindle
by replacing the old session and immediately resending the last frame, so a
reconnect does not leave a blank panel.

**Gestures are recognised on the Kindle, not streamed.** A short touch
becomes `TAP x y`; a vertical drag becomes one `SCROLL dy`. Streaming raw
motion events would swamp the link for no visible benefit.

## Build

### Kindle daemon

```
cd kindle
make            # static armv7 binary, no cgo, nothing to install alongside it
make test       # unit tests, race detector on
```

`make arm64` builds for a 64-bit userland instead. The daemon has no
dependencies, so a Kindle only ever needs the one file.

### Android app

The app has two flavours. They share everything except whether the Kindle
can control the phone or only watch it — see [Which build to install](#which-build-to-install).

```
cd android
./gradlew assembleMirrorDebug    # app/build/outputs/apk/mirror/debug/
./gradlew assembleControlDebug   # app/build/outputs/apk/control/debug/
./gradlew assemble               # everything

./gradlew testMirrorDebugUnitTest testControlDebugUnitTest
./gradlew lintMirrorDebug lintControlDebug
```

Needs the Android SDK (platform 35, build-tools 35) and a JDK 17+.

## Releases

Both artifacts come out of the same workflow: `.github/workflows/build.yml`
builds the Go binaries and the APK, CI calls it on every push, and the
release workflow calls the identical thing on a tag. A release is never
built differently from what CI already proved.

Cutting one:

```
git tag v0.1.0
git push origin v0.1.0
```

That publishes `kindled-<version>-linux-armv7`,
`kindled-<version>-linux-arm64`, the APK and a `SHA256SUMS` file. The
version is stamped into both artifacts from the same tag -- `kindled
-version` prints it, and it becomes the APK's `versionName`, with a
`versionCode` derived from the semver so upgrades are accepted.

`workflow_dispatch` builds a release by hand if you would rather not tag.

### Signing the APK

With no signing secrets configured the release APK is published unsigned
rather than failing the build, alongside a debug-signed one you can install
directly. To get a signed release APK, create a keystore and set four
repository secrets:

```
keytool -genkey -v -keystore release.jks -keyalg RSA \
    -keysize 2048 -validity 10000 -alias kindled
base64 -w0 release.jks     # -> ANDROID_KEYSTORE_BASE64
```

| Secret | Value |
| --- | --- |
| `ANDROID_KEYSTORE_BASE64` | the keystore, base64 encoded |
| `ANDROID_KEYSTORE_PASSWORD` | keystore password |
| `ANDROID_KEY_ALIAS` | key alias (`kindled` above) |
| `ANDROID_KEY_PASSWORD` | key password |

Keep the keystore. Android only accepts an upgrade signed with the same key.

## Install

### On the Kindle

```
./scripts/deploy.sh 192.168.15.244      # builds and copies over SSH
```

then on the device:

```
/mnt/us/kindled/kindled -version
/mnt/us/kindled/kindled-start.sh -v     # logs to /mnt/us/kindled.log
```

`kindled-start.sh` waits for a default route before starting, and restarts
the daemon if it ever exits.

If the Kindle framework repaints over the streamed image, stop it
(`stop lab126_gui`, and `start lab126_gui` or reboot afterwards). There is a
commented line for this in `kindled-start.sh`.

### Which build to install

| | `mirror` | `control` |
| --- | --- | --- |
| Kindle shows the phone's screen | yes | yes |
| Kindle taps and scrolls do something | **no** | yes |
| Installs normally | **yes** | no — Play Protect blocks it |
| Declares an accessibility service | no | yes |

**Start with `mirror`.** It installs like any other APK and proves the whole
chain — hotspot, link, frames, e-ink refresh — without a fight.

Injecting a touch requires an accessibility service, and that declaration
alone is enough for Play Protect to refuse a sideloaded install with *"This
app can request access to sensitive data… identity theft or financial
fraud"*. It is reacting to the manifest, not to anything in the code, and a
properly signed release APK is blocked exactly the same way. So the mirror
build ships without any accessibility service at all — no manifest entry and
no class in the DEX, which CI asserts on every build.

A mirror-only phone advertises `no-input` in its `READY` line, so the Kindle
stops sending gestures rather than firing them into a phone that will drop
them. See [PROTOCOL.md](PROTOCOL.md#capabilities).

### On the Pixel

1. Install the APK — for `mirror`, the normal way:

   ```
   adb install -r kindle-display-<version>-mirror-debug.apk
   ```

   Use a `-debug` APK unless you configured signing secrets; an `-unsigned`
   one cannot be installed at all.

2. Grant **Display over other apps** — needed only for the screen blackout.
3. Turn on the hotspot, join the Kindle to it, pick an app, press Start.

#### Installing the control build

Sideloading it needs two extra steps, and neither is optional:

1. Install over ADB. The Play Protect block targets installs from browsers,
   file managers and messaging apps; `adb install` is the way through.

   ```
   adb install -r kindle-display-<version>-control-debug.apk
   ```

   If Play Protect still refuses: Play Store → profile → Play Protect → ⚙ →
   turn off scanning, install, turn it back on.

2. Unlock the accessibility toggle. Android hides it for sideloaded apps:
   **Settings → Apps → Kindle Display → ⋮ → Allow restricted settings**,
   then **Settings → Accessibility → Kindle Display → enable**.

Without step 2 the app installs and runs, but taps and scrolls arrive and go
nowhere. Menu wording moves around between Android versions.

## Travel workflow

1. Turn on the Pixel hotspot.
2. The Kindle joins it automatically (save the SSID on the Kindle once).
3. `kindled` reconnects on its own.
4. Open Kindle Display on the Pixel and press Start.
5. Black out the phone screen.
6. Read on the Kindle.

No internet is required for any of this. The link is a single TCP connection
across the hotspot's own subnet.

## Tuning

The Kindle daemon's defaults suit a 1072x1448 panel:

```
kindled -h
  -addr           Pixel address; default is the hotspot gateway
  -backend        eips (default), fbink, or none
  -idle-refresh   quiet period before a clean full refresh (default 500ms)
  -full-every     force a full refresh every N frames (default 16)
  -touch-swap-xy / -touch-invert-x / -touch-invert-y
  -tap-slop / -tap-max / -scroll-min
  -v              log every frame, gesture and reconnect
```

If the touchscreen axes come out wrong, the three orientation flags cover
every mounting. If eips is not good enough, `-backend fbink` swaps the
display implementation and nothing above it changes.

## Testing without hardware

`fakepixel` speaks the server half of the protocol and streams a moving test
pattern, so the Kindle daemon can be exercised end to end before the phone
is involved:

```
cd kindle
make fakepixel
./fakepixel -listen :45831
./kindled -addr 127.0.0.1 -backend none -v      # on a dev machine
```

## Status

Verified here:

* The Kindle daemon end to end against `fakepixel`: handshake, JPEG frames
  written and handed to a stubbed `eips` with alternating buffers, ACKs,
  full refresh every N frames, one clean refresh when the link goes quiet.
* Frame dropping under a deliberately slow panel: 8 stale frames dropped in
  a 7 s run, always keeping the newest.
* Reconnect after the server disappears and comes back.
* Cross-compilation to a static armv7 ELF binary.
* The Android app builds, lints clean, and passes 10 protocol tests against
  a socket that behaves like `kindled`.

**Not verified, because it needs the actual devices:**

* eips behaviour on a real 2024 Kindle — refresh quality, achievable frame
  rate, and whether `eips -g` accepts JPEG on that firmware. If it does not,
  either convert to PNG on the Pixel or switch to `-backend fbink`.
* Touchscreen device node, axis ranges and orientation on the real panel.
* Whether Android lets this app launch a *third-party* activity onto a
  virtual display it owns. This is the one genuinely uncertain step: the
  system may refuse with a `SecurityException`, which the app surfaces as
  "Launch refused". Some devices need the developer option **Force activities
  to be resizable** and/or **Enable freeform windows** to allow it.
* Real-world power draw with the screen blacked out.

### On keeping the Pixel's OLED off

The obvious approach — let the phone sleep — does not work. A virtual
display without the system's trusted-display privilege lives in the default
display group, and that group sleeps when the physical screen does, taking
the Kindle's frames with it. `VIRTUAL_DISPLAY_FLAG_OWN_DISPLAY_GROUP` would
fix it properly but requires a signature-level permission.

So the app makes the OLED cost nothing rather than turning it off: a black
overlay at minimum brightness, with a partial wakelock keeping the CPU up.
On an OLED, black pixels are unlit. If you are rooted, granting
`ADD_TRUSTED_DISPLAY` and using an own-display-group virtual display is the
better answer.
