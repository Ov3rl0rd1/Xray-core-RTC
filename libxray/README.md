# libxray

Xray-core built as a C shared library, for embedding in a client app rather
than shipping the `xray` executable and managing a child process.

Built by `.github/workflows/build-lib.yml`:

```
android/arm64-v8a/libxray.so     windows/x64/xray.dll
android/armeabi-v7a/libxray.so   windows/x86/xray.dll
android/x86_64/libxray.so
android/x86/libxray.so
```

A `libxray.h` / `xray.h` sits next to each binary. C and C++ callers want it;
P/Invoke callers do not need it. Headers are kept per target because the `GoInt`
typedefs differ between 32- and 64-bit builds.

On Android, drop the `.so` files into `src/main/jniLibs/<abi>/` (or a .NET
Android project's `@(AndroidNativeLibrary)` items) so the ABI layout is
preserved — the loader picks the right one at install time.

## API

Every `int` function returns `0` on success and `-1` on failure; call
`XrayLastError` for the message. Every `char*` returned is owned by the caller
and **must** be released with `XrayFree` — it comes from C `malloc`, so
`Marshal.FreeHGlobal` is not the right pairing.

| Function | Meaning |
| --- | --- |
| `int XrayStart(const char* configJSON)` | Parse the JSON config, build the instance and start it. Fails if an instance is already running. |
| `int XrayStop(void)` | Stop and release the running instance. Not an error when nothing is running, so teardown paths can call it unconditionally. |
| `int XrayIsRunning(void)` | `1` when an instance is running, `0` otherwise. |
| `int XrayTest(const char* configJSON)` | Parse the config and build every handler without starting anything — the equivalent of `xray run -test`. |
| `int XraySetAssetPath(const char* path)` | Directory holding `geoip.dat` and `geosite.dat`. Required before `XrayStart` if any routing rule uses `geoip:` or `geosite:`. |
| `int XrayResetConnections(void)` | Close pooled transport sessions so the next dial builds fresh ones; returns how many were closed. See below. |
| `void XrayForceGc(void)` | Return freed memory to the OS. Asynchronous, so it is safe from a low-memory callback. |
| `int XraySleep(void)` | Pause background housekeeping while the device is idle. Idempotent. |
| `int XrayWake(void)` | Resume it. Idempotent, and safe without a matching `XraySleep`. |
| `int XrayIsPaused(void)` | `1` paused, `0` running, `-1` on failure. Diagnostics. |
| `char* XrayVersion(void)` | Version string. |
| `char* XrayLastError(void)` | Message from the last failed call; empty string if the last call succeeded. |
| `void XrayFree(char* s)` | Release a string returned by this library. |

### Panics do not reach the host

Every exported function recovers, so a panic inside the core returns `-1` with
the message and a stack in `XrayLastError` instead of calling `abort()` and
taking the host process down with it. Runtime *fatals* — concurrent map writes,
deadlock detection, out of memory — are not panics and remain unrecoverable;
those still abort, and the signature is a process death with nothing in the
host's crash log.

### `XrayResetConnections`

For network handovers. Every session established over the previous link dies
with it, but nothing in the stack is told so: QUIC sits on the dead path until
an idle timeout measured in minutes, which the user experiences as a VPN that
is connected and carries nothing until they toggle it off and on.

Closing the sessions makes the next request rebuild on the new path at once,
and it is far cheaper than the alternative a host would otherwise reach for —
the running instance and the host's TUN stay up, and only what the handover
actually invalidated is discarded. Safe with nothing running; returns 0.

Covers the hysteria (QUIC) pool, which is where the long-lived sessions are.
VLESS over TCP is dialled per request and needs nothing.

### `XraySleep` / `XrayWake`

A core embedded in a mobile client keeps running when the screen goes off, and
its housekeeping goroutines do not know that. The hysteria transport alone runs
two at 1 Hz — reaping idle UDP sessions, reaping dead QUIC clients — and on
Android every tick is a timer the kernel services, which is one of the things
that stops a device settling into deep sleep. Over a night that is tens of
thousands of wakeups to look at a map that has not changed.

`XraySleep` stops those timers; `XrayWake` restarts them. Traffic is untouched
— only housekeeping is affected, and the reaping it defers is caught up by the
first tick after the wake. Drive them from the platform's own idle signal
(`ACTION_DEVICE_IDLE_MODE_CHANGED` on Android). A host that never calls them
gets exactly the previous behaviour.

The config is the same JSON Xray's CLI takes, so a chain of
`hysteria` → `vless` → `olcrtc` outbounds is expressed exactly as it would be in
`config.json`.

## P/Invoke

```csharp
using System.Runtime.InteropServices;

internal static partial class Xray
{
    // Android: "xray" resolves libxray.so. Windows: xray.dll.
    private const string Lib = "xray";

    [LibraryImport(Lib, StringMarshalling = StringMarshalling.Utf8)]
    internal static partial int XrayStart(string configJson);

    [LibraryImport(Lib)]
    internal static partial int XrayStop();

    [LibraryImport(Lib)]
    internal static partial int XrayIsRunning();

    [LibraryImport(Lib, StringMarshalling = StringMarshalling.Utf8)]
    internal static partial int XrayTest(string configJson);

    [LibraryImport(Lib, StringMarshalling = StringMarshalling.Utf8)]
    internal static partial int XraySetAssetPath(string path);

    [LibraryImport(Lib)]
    internal static partial IntPtr XrayVersion();

    [LibraryImport(Lib)]
    internal static partial IntPtr XrayLastError();

    [LibraryImport(Lib)]
    internal static partial void XrayFree(IntPtr s);

    // Takes ownership of a char* the library returned and frees it.
    private static string Consume(IntPtr p)
    {
        if (p == IntPtr.Zero) return string.Empty;
        try { return Marshal.PtrToStringUTF8(p) ?? string.Empty; }
        finally { XrayFree(p); }
    }

    internal static string Version() => Consume(XrayVersion());

    internal static void Start(string configJson)
    {
        if (XrayStart(configJson) != 0)
            throw new InvalidOperationException(Consume(XrayLastError()));
    }
}
```

Note that the .so and .dll differ in name (`libxray.so` vs `xray.dll`), which is
what the platforms expect: `"xray"` resolves to both under the usual probing
rules.

## Feature set

The library links the feature set in `main/distro/lib`, which is
`main/distro/all` minus the CLI command tree, the TOML and YAML config formats
and the remote config loader.

The protocol set is *not* reduced, and cannot easily be. `infra/conf` — the JSON
config parser — imports every proxy and transport package for its config types,
and importing a Go package runs its `init()`, which is where handlers register.
So accepting JSON config links every protocol in regardless of what
`main/distro/lib` lists. Narrowing the protocol set means giving up JSON config
and having the caller pass a serialized protobuf `core.Config` instead.

## Not included

There is no socket-protect hook. On Android a `VpnService` needs outbound
sockets protected from its own tunnel, or traffic loops; wiring that up means
exposing a callback that reaches `internet.RegisterDialerController`. Add it
before using this in a `VpnService`.
