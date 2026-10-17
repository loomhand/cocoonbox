# Android box

This box is an Ubuntu 24.04 VM without a desktop that runs Android 16 (720x1280) in docker. The agent's screen tools act on Android.

- adb on PATH reaches the phone (`ANDROID_SERIAL=127.0.0.1:5555`): `adb shell`, `adb install app.apk`, `adb push`/`pull`, `adb shell am start -n <package>/<activity>`, `adb shell pm list packages`, `adb shell settings`.
- `adb exec-out screencap -p > shot.png` saves a screenshot; `adb shell uiautomator dump` the UI tree.
- Android reaches the internet only through its HTTP proxy 172.18.0.1:3128; DNS works, direct connections do not.
- Android's `/data` lives in the VM at `/var/lib/redroid-data`.
