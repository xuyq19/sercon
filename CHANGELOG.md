# Changelog

All notable changes to sercon are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
releases use [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-09-10

First working version. Verified end to end against real hardware rather than
built and hoped for.

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
