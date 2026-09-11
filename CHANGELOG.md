# Changelog

All notable changes to sercon are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
releases use [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-09-11

The Windows window is no longer a stock control. The client area is painted by
hand, which is what the sidebar, the state capsules and the transitions needed;
nothing outside the window changed.

### Added

- **The Windows GUI is drawn by hand.** A dark sidebar and a light content area,
  a port table with rounded state capsules, four statistics, and hover and
  selection feedback that eases rather than snapping. Geometry is computed once
  per resize and read by both the paint and the hit-test paths, so a control
  cannot be drawn in one place and clickable in another.
- **Transitions**: a per-entry sidebar highlight, a row highlight, a row that
  fades and shrinks when an adapter goes away, and statistics that roll to their
  new value. One shared timer drives all of them and is only installed while
  something is moving, so an idle window schedules no frames.
- **Fonts by role**: Segoe UI for the interface, Consolas for numbers and paths.
  Table columns are measured rather than counted, because Segoe UI is
  proportional.
- **SSH key panel in the Windows GUI.** A window that installs a client's public
  key so it can reach the machine. It picks the file sshd actually reads — for an
  administrator that is `%ProgramData%\ssh\administrators_authorized_keys`, not
  `~/.ssh/authorized_keys` — tightens the ACL sshd requires, and raises a UAC
  prompt for the write. The GUI itself still runs un-elevated; only this one
  action elevates.
- **`sercon attach` accepts a pipe**, so the remote console behaves like the local
  device: `sercon attach ... < /dev/null | grep -m1 panic` reads until the match,
  and `< /dev/null` alone reads until interrupted, the same as `cat /dev/ttyS0`.
  Status notes move to stderr so they cannot corrupt the stream, the escape key
  and raw mode are skipped (every byte is meant literally), and reconnecting is
  suppressed because a shell pipeline that ends should end.
- **`hack/gui-drive.py`** for driving the GUI from a script, and
  `hack/screenshot-window.py` now takes a window class so it can capture the key
  panel as well as the main window.
- **`hack/shoot-demo.py`, `hack/shoot-burst.py`, `hack/cpu-slices.py` and
  `hack/win-probe.py`** for verifying the window without a person watching it:
  a scripted tour of the animations, burst sampling of a single transition, CPU
  sampled in slices, and window geometry.

### Changed

- **The port table's log column shows the file name only.** The directory is the
  same for every port, so the full path repeated itself down the column and used
  most of the width.
- **The sidebar has one caption instead of four.** It had four section captions
  for four items in a single section, which was three captions standing in for
  functionality that does not exist.
- **The GUI's idle cost is roughly a fifth of what it was.** The previous window
  repainted its whole table on every one-second tick; this one repaints only when
  the port table, the row set or the footer text actually differs. Measured over
  the same 36 seconds on the same machine: 234 ms of process CPU before, 47 ms
  after. What remains is the daemon's serial polling, which predates the change.

### Removed

- **Every leftover from the control-based window**: `ImageList_Create` and the
  empty image list that existed only to give a ListView room to breathe,
  `InitCommonControlsEx`, `SetWindowTheme`, `WM_NOTIFY`, the whole `LVM_*` range,
  the `NMLVCUSTOMDRAW` structs and their `CDDS_*`/`CDRF_*` constants. `comctl32.dll`
  and `uxtheme.dll` are no longer loaded at all; comctl32 v6 still arrives through
  the manifest, which is what themes the key panel's controls.

### Fixed

- **The SSH key panel could not be opened at all.** `RegisterClassExW` reports an
  already-registered class as a failure, and that was being reported as an error,
  so the second registration of the panel's class aborted it before it created a
  window. The only symptom was that the panel never appeared — its failure path
  was a dialog parented to a window that did not exist. Registering a class twice
  is now treated as the no-op it is, and the redundant startup registration is
  gone.
- **The build version was drawn twice over.** In a stamped build the full identity
  is forty-odd characters, and it was being drawn in the sidebar footer, 168px
  wide, where it ran over the buttons, and in the bottom bar's right-hand slot
  next to the socket path. Both now show the commit, which fits and identifies
  the build; `--version` and the daemon log line keep the full string.
- **The version repeated its own commit hash.** `Full()` suppressed the appended
  commit only when the version equalled it, which is the pre-tag case. Once a tag
  exists `git describe` returns `v0.1.0-9-gabc1234`, which is not equal to the
  hash but already ends with it, so the hash appeared twice:
  `v0.1.0-9-gabc1234+abc1234`.
- **The last table column could extend past the table.** A 120px minimum on the
  log column pushed its right edge outside the clipped region in a narrow window,
  where its right-aligned text would have been drawn outside the table.
- Two bugs in `internal/sshauth`, both found by testing against a real account
  database rather than a fixture: `IsAdmin("")` answered false for an un-elevated
  administrator, because UAC hands such a process a filtered token with the group
  marked deny-only and `CheckTokenMembership` reports that as absent — so the
  panel would have pointed at the wrong file, which is exactly the failure the
  package exists to prevent. `Remove` also re-rendered the file even when nothing
  matched, rewriting line endings for no reason.

## [0.1.0] - 2026-09-10

First working version. Driven against real hardware on both platforms.

### Added

- **Daemon** (`sercond capture`) that owns the serial devices on a jump host,
  writes per-port logs whether or not anyone is attached, and serves a socket.
- **Client** (`sercon`) with `ls`, `attach`, `run`, `status` and `stop`.
- **Windows GUI daemon** (`sercon-gui.exe`): a native Win32 window over the
  same daemon, with the port table, a colour per state, and buttons for the log
  directory and for copying an attach command.
- **Two serial backends**: Linux termios + epoll, and the Windows Win32
  communications API with overlapped I/O.
- **Session model**: one writer per port with any number of read-only observers,
  a 64 KiB backlog replayed on attach, and a bounded per-connection send queue
  so one stalled client cannot stall the port.
- **Unattended logging**: per-port logs rotate by day, append-only, with a
  self-describing header; a JSONL audit trail records every session, lock and
  device transition.
- **Detachment without a service manager**: `setsid` on Linux and
  `CREATE_BREAKAWAY_FROM_JOB` on Windows, so the daemon outlives the SSH session
  that started it.
- `sercon run` for scripted sessions (`send`, `sendln`, `wait`, `sleep`).

### Verified on real hardware

On a Linux machine with two FTDI USB serial adapters, one of them cabled to a
BMC's serial console:

- the daemon survives the SSH session that started it;
- both adapters enumerate through `/dev/serial/by-id` and open at 115200 8N1;
- live BMC kernel output is captured into the per-port log;
- unbinding an adapter from the USB driver — the kernel's equivalent of pulling
  the plug — is detected as offline, and the port returns on its own within
  seconds, in the same log file, with no gap;
- an interactive session renders the console to a real terminal over SSH.

### Known limitations

- The write direction is unconfirmed on Linux: both adapters were cabled to
  devices that do not echo and there was no loopback plug, so only "the write
  call succeeded" is proven there.
- Unplug detection on Windows is implemented but has not been exercised against
  a real unplug.
- Windows has no stable serial identity, so a re-plugged adapter either keeps
  its COM number or gets a new one, and a new number starts a new log file.
- The relay transport in `internal/relay` is implemented but deliberately not
  wired into the command line. It has never been run against a real NAT.
