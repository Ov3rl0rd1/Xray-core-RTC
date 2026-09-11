# How this fork tracks upstream

This repository is **XTLS/Xray-core plus five features**. Keeping those features
alive across upstream releases used to mean merging and resolving conflicts by
hand; it is now mechanical, because the fork is stored as two things that behave
very differently:

| | What | Can it conflict? |
|---|---|---|
| **Overlay** | `fork/manifest.txt` — ~80 files that exist only here | **Never.** Upstream has no such files. |
| **Patches** | `fork/patches/` — the handful of lines we need *inside* upstream files | Only these. |

The whole design goal is to keep the second column as small as possible. It is
currently **6 files and about 30 lines of actual code**, and four of the six
patches are one-liners.

`go.mod` / `go.sum` are deliberately in neither. They are the largest and most
conflict-prone files in any Go fork — 376 lines of guaranteed conflict per
update — and they do not need to be tracked at all: `fork/deps.txt` pins the
versions we care about, and `go mod tidy` derives the rest.

## What the fork adds

| Feature | Overlay | Touches upstream |
|---|---|---|
| **olcRTC** — TCP-over-WebRTC proxy | `proxy/olcrtc/**`, `proxy/selfdriven.go`, `app/proxyman/inbound/selfdriven.go`, `infra/conf/olcrtc.go`, `infra/conf/fork_registry.go`, `main/distro/all/fork.go` | 6 lines in `inbound.go`, 1 in `infra/conf/xray.go` |
| **Per-user traffic shaping** | `common/shaper/**`, `app/dispatcher/ratelimit.go` | 5 lines in `app/dispatcher/default.go` |
| **Plans, quotas and usage** (+ its gRPC API) | `app/tariff/**`, `app/dispatcher/fork_usage.go` | 1 line in `infra/conf/api.go` |
| **Pause/resume housekeeping** (battery, for the mobile client) | `common/pause/pause.go`, `transport/internet/hysteria/fork_pause.go` | 3 lines across `hysteria/conn.go` + `dialer.go` |
| **libxray** — C ABI for the mobile client | `libxray/**`, `main/distro/lib/lib.go` | — |

Everything is written to plug in through its own file wherever Go allows it. The
pause feature is the clearest example: it started as 158 lines edited into two
upstream files that upstream rewrites regularly (the Hysteria v2.12.2 upgrade
touched both), and conflicted immediately. Moving the logic into
`fork_pause.go` and leaving three call sites behind reduced it to nothing that
can realistically conflict.

**When adding a feature, follow that pattern**: put the code in a new file, and
leave at most a call in upstream's.

## Two upstreams, two tools

This fork tracks **two** upstreams, and each has its own tool built on the same
idea — a manifest of what we own, patches for the few lines we need inside
theirs, and a recorded ref:

| | Upstream | Tool | Manifest | Patches |
|---|---|---|---|---|
| Xray | `XTLS/Xray-core` | `fork/bin/fork` | `fork/manifest.txt` | `fork/patches/` (6, ~35 lines) |
| olcRTC | `openlibrecommunity/olcrtc` | `fork/bin/olcrtc` | `fork/olcrtc-manifest.txt` | `fork/olcrtc-patches/` (13, ~540 lines) |

The olcRTC one exists because nothing was watching that copy: by the time
anyone looked, 41 of its 43 vendored files had changed upstream and 8 were
gone. `./fork/bin/olcrtc diff <checkout>` answers "has this drifted?" in
seconds, which is the question nobody could previously ask.

## The tool

```
./fork/bin/fork status              # what we are on, what upstream has released
./fork/bin/fork check <ref>         # does the fork still apply, build and test?
./fork/bin/fork adopt <ref>         # land it on a branch for review
./fork/bin/fork export              # regenerate patches after editing code
./fork/bin/fork build <ref> <dir>   # construct a tree somewhere, for poking at
```

### Updating to a new upstream release

```bash
./fork/bin/fork status              # "upstream has released v26.8.4"
./fork/bin/fork check v26.8.4       # applies? builds? tests?
./fork/bin/fork adopt v26.8.4       # -> branch update/v26.8.4
git diff HEAD..update/v26.8.4       # this diff is exactly what upstream changed
git checkout main && git merge --ff-only update/v26.8.4
```

`adopt` lands a single merge commit whose tree is the verified build and whose
parents are (this fork, that upstream release), so the fork's history survives
and git knows the release is merged. `main` is never touched until you say so.

### When something conflicts

```bash
./fork/bin/fork build v26.8.4 /tmp/trial   # conflicts are left in the tree
cd /tmp/trial
git diff --diff-filter=U                   # what argued
# fix the files, then regenerate the patches from the fixed tree:
cp <fixed files> back into this checkout
./fork/bin/fork export
```

Consider whether the conflict is telling you the patch should not exist —
whether the code can move into an overlay file with a smaller call site instead.
That is usually the better fix, and it is permanent.

### Changing fork code

Edit normally, then:

```bash
./fork/bin/fork export
```

If you edited an **upstream** file, also add it to `fork/patch-groups.txt`.
CI (`fork-guard.yml`) fails the build if the patches do not match the tree, or if
a modified upstream file belongs to no group — that is the one way this layout
can still lose a change silently.

## Automation

| Workflow | When | Does |
|---|---|---|
| `fork-guard.yml` | every push / PR | patches match the tree; every touched upstream file is accounted for |
| `upstream-watch.yml` | Mondays, or manually | tries the fork against the newest upstream **release**, builds, vets, tests, and opens one issue with the verdict |

`upstream-watch` **never pushes anything**. It reports; you decide. That is
deliberate for a VPN data plane: an update that lands unattended is an outage
you find out about from users.

It follows release tags rather than `main`, because `main` moves daily and half
of it never ships.

## Files

```
fork/
  upstream.txt          the Xray release this fork is currently based on
  manifest.txt          paths the fork owns (the overlay)
  patch-groups.txt      which upstream file goes into which patch
  deps.txt              direct dependencies, pinned; go mod tidy derives the rest
  patches/              generated by `fork export` — do not hand-edit
  bin/fork              the Xray tool

  olcrtc.txt            the olcRTC commit the vendored library is at
  olcrtc-manifest.txt   what is vendored, skipped, and ours inside that copy
  olcrtc-patches/       generated by `olcrtc export` — do not hand-edit
  bin/olcrtc            the olcRTC tool
```

### Adding a file inside the vendored olcRTC tree

List it in `fork/olcrtc-manifest.txt` as an `=` entry **before** the next
vendor, or it is deleted along with everything else that is replaced.
`olcrtc vendor` refuses to run when it finds a file that is neither upstream's
nor claimed by the manifest, which is what stops that happening quietly.
