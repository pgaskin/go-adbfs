# go-adbfs

Go [fs.FS](https://pkg.go.dev/io/fs#FS) implementation for a connected Android device via the [adb daemon](https://developer.android.com/tools/adb) without using shell commands.

> [!WARNING]
> This library is still in development. While most functionality works, error handling (especially connection, not found, and permission errors) is still somewhat lacking, and the API is subject to change.
