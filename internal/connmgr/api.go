// Package connmgr defines the public interfaces,
// responsible for preparing Unix FDs for RFCOMM SPP connections via BlueZ D-Bus.
//
// Thread-safety: except for Close(), methods are not safe for concurrent use.
// Callers must serialize StartServer, Accept, ScanSPP, and Connect. Close is
// safe to call concurrently and is idempotent.
package connmgr

import (
    "context"
)

const (
    // SPPUUID is the Serial Port Profile UUID used for RFCOMM connections.
    SPPUUID = "00001101-0000-1000-8000-00805f9b34fb"

    // DefaultRFCOMMChannel is the fixed RFCOMM channel for the server-side profile.
    DefaultRFCOMMChannel uint8 = 22
)

// Device represents the minimum information needed to display and connect.
//
// Path is required (BlueZ Device1 object path as string). Other fields are optional
// and may be empty depending on discovery results.
type Device struct {
    Path        string // required: D-Bus object path of the device (e.g. /org/bluez/hci0/dev_XX_XX_XX_XX_XX_XX)
    MAC         string // optional: Bluetooth device address
    Name        string // optional: Device1.Name
    Alias       string // optional: Device1.Alias
    ServiceName string // optional: SDP ServiceName (0x0100) if available
}

// ServerOptions controls server-side profile registration.
type ServerOptions struct {
    // ServiceName is required and will be used for RegisterProfile options["Name"].
    ServiceName string
}

// Mgr is the single public interface for discovery and connections.
// Responsibilities end at preparing FDs for the caller; reconnect is out of scope.
type Mgr interface {
    // StartServer registers an SPP profile (Role="server").
    // After a successful call, use Accept to wait for exactly one incoming connection.
    // State/usage constraints:
    //   - Must be called before Accept; calling Accept without a prior StartServer returns an error.
    //   - Calling StartServer more than once returns an error.
    //   - A Mgr instance is single-role: if Connect has been used on this instance,
    //     StartServer returns an error (and vice versa).
    //   - If the fixed RFCOMM Channel is already in use, an error is returned.
    StartServer(ctx context.Context, opts ServerOptions) error

    // Accept blocks until a connection is established or ctx is canceled.
    // It returns the peer device information and a Unix file descriptor (FD) that the caller owns.
    // The caller should wrap the FD with os.NewFile(uintptr(fd), "rfcomm") for I/O and must Close it.
    // Server semantics and state/usage constraints:
    //   - Accept may be called at most once. Multiple connections or re-listen are not supported.
    //   - After one connection has been accepted, any subsequent incoming connections must be rejected
    //     or their FDs immediately closed by the implementation.
    //   - Once Accept has returned an FD, the implementation must NOT close that FD later due to ctx
    //     cancellation or other internal events; ownership is entirely with the caller.
    //   - If called before StartServer or after Close, returns an error.
    // remote resolution:
    //   - The implementation should attempt to provide the peer's MAC at minimum.
    //     If the peer information cannot be resolved at accept time, return the zero-value Device.
    Accept(ctx context.Context) (fd int, remote Device, err error)

    // ScanSPP discovers nearby devices advertising SPP and returns a snapshot list.
    // Only devices containing SPPUUID are included. Implementations may attempt to obtain SDP ServiceName
    // for better display.
    // Timing control is by the caller-provided context; use context.WithTimeout as needed.
    // Contract:
    //   - Each returned Device must have a non-empty Path.
    //   - May be called in any state except after Close; after Close returns an error.
    ScanSPP(ctx context.Context) ([]Device, error)

    // Connect initiates an outgoing connection to the given device.
    // A client-side profile (Role="client") is registered internally as needed.
    // If pairing is required, a pre-registered BlueZ Agent (external to this package) must handle it.
    // Then it waits for Profile1.NewConnection to obtain an FD. The returned FD is owned by the caller.
    // State/usage constraints:
    //   - The provided dev.Path must be non-empty; if empty, returns an error immediately.
    //   - A Mgr instance is single-role: if StartServer/Accept has been used on this
    //     instance, Connect returns an error.
    //   - Connect may be called at most once per manager instance.
    // Error policy:
    //   - Context cancellation and deadlines are propagated: errors wrapping context.Canceled or
    //     context.DeadlineExceeded may be returned.
    Connect(ctx context.Context, dev Device) (fd int, err error)

    // Close releases resources held by the manager (e.g., D-Bus objects, signal subscriptions).
    // Contract:
    //   - Safe for concurrent use; redundant calls are allowed (idempotent).
    //   - After Close, all other methods return an error.
    Close() error
}
