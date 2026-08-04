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
| `char* XrayVersion(void)` | Version string. |
| `char* XrayLastError(void)` | Message from the last failed call; empty string if the last call succeeded. |
| `void XrayFree(char* s)` | Release a string returned by this library. |

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
