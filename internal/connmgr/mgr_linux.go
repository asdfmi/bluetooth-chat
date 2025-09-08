//go:build linux

package connmgr

import (
    "context"
    "errors"
    "fmt"
    "os"
    "strconv"
    "strings"
    "sync"
    "sync/atomic"

    dbus "github.com/godbus/dbus/v5"
)

// New creates a new manager instance.
func New() Mgr {
    return &mgr{}
}

// Note: no sentinel errors are exposed; callers should inspect returned errors as needed.

type role int

const (
    roleNone role = iota
    roleServer
    roleClient
)

const (
    bluezService         = "org.bluez"
    profileInterfaceName = "org.bluez.Profile1"
    profileManagerIface  = "org.bluez.ProfileManager1"
    deviceIface          = "org.bluez.Device1"
    adapterIface         = "org.bluez.Adapter1"
    objManagerIface      = "org.freedesktop.DBus.ObjectManager"
    propsIface           = "org.freedesktop.DBus.Properties"
)

var pathCounter uint64

type mgr struct {
    mu     sync.Mutex
    closed bool

    bus *dbus.Conn

    role role

    // server state
    serverExported bool
    acceptUsed     bool
    srvProf        *profile
    serverPath     dbus.ObjectPath

    // client state
    clientExported bool
    connectUsed    bool
    cliProf        *profile
    clientPath     dbus.ObjectPath

    // cleanup functions to release resources in Close (executed once, in reverse order).
    cleanup []func()
}

// ensureBusLocked connects to the system bus if not yet connected.
func (m *mgr) ensureBusLocked() error {
    if m.bus != nil {
        return nil
    }
    c, err := dbus.SystemBus()
    if err != nil {
        return fmt.Errorf("connmgr: connect system bus: %w", err)
    }
    m.bus = c
    // Close the bus last during cleanup.
    m.cleanup = append(m.cleanup, func() { m.bus.Close() })
    return nil
}

// profile implements org.bluez.Profile1 and forwards NewConnection events.
type profile struct {
    ch       chan acceptResult // non-nil while accepting/connecting
    accepted bool              // true after first delivery; subsequent connections are rejected/closed
}

type acceptResult struct {
    fd  int
    dev Device
    err error
}

// Release is called by BlueZ when the profile is being released.
func (p *profile) Release() *dbus.Error { return nil }

// Cancel may be called to indicate a canceled request.
func (p *profile) Cancel() *dbus.Error { return nil }

// RequestDisconnection is ignored in this minimal implementation.
func (p *profile) RequestDisconnection(_ dbus.ObjectPath) *dbus.Error { return nil }

// NewConnection delivers the incoming RFCOMM socket FD to the waiting goroutine.
func (p *profile) NewConnection(dev dbus.ObjectPath, fd dbus.UnixFD, _ map[string]dbus.Variant) *dbus.Error {
    res := acceptResult{
        fd: int(fd),
        dev: Device{
            Path: string(dev),
            MAC:  macFromPath(dev),
        },
        err: nil,
    }
    // Non-blocking delivery with single-accept guarantee.
    if p.accepted {
        // Already accepted once; close FD and reject.
        _ = os.NewFile(uintptr(res.fd), "rfcomm").Close()
        return &dbus.Error{Name: "org.bluez.Error.Rejected", Body: []interface{}{"already accepted"}}
    }
    select {
    case p.ch <- res:
        p.accepted = true
        return nil
    default:
        // No receiver; close FD and return a rejection to avoid leaks.
        _ = os.NewFile(uintptr(res.fd), "rfcomm").Close()
        return &dbus.Error{Name: "org.bluez.Error.Rejected", Body: []interface{}{"no receiver"}}
    }
}

func (m *mgr) StartServer(ctx context.Context, opts ServerOptions) error {
    _ = ctx // ctx reserved; registration is fast and not cancellable via D-Bus API directly.
    m.mu.Lock()
    defer m.mu.Unlock()
    if m.closed {
        return errors.New("connmgr: closed")
    }
    if m.role == roleClient || m.connectUsed {
        return errors.New("connmgr: already used as client")
    }
    if m.serverExported {
        return errors.New("connmgr: server already started")
    }
    if err := m.ensureBusLocked(); err != nil {
        return err
    }

    if opts.ServiceName == "" {
        return errors.New("connmgr: ServiceName required")
    }

    // Export Profile1 for server role.
    m.srvProf = &profile{ch: make(chan acceptResult, 1)}
    // Unique object path per instance to avoid collisions.
    id := atomic.AddUint64(&pathCounter, 1)
    m.serverPath = dbus.ObjectPath("/org/bluetooth_chat/connmgr/server/p" + strconv.FormatUint(id, 10))
    if err := m.bus.Export(m.srvProf, m.serverPath, profileInterfaceName); err != nil {
        return fmt.Errorf("connmgr: export server profile: %w", err)
    }
    m.serverExported = true

    // Register the profile with BlueZ.
    optsMap := map[string]dbus.Variant{
        "Name":    dbus.MakeVariant(opts.ServiceName),
        "Role":    dbus.MakeVariant("server"),
        // BlueZ expects Channel as a uint16 (not byte).
        "Channel": dbus.MakeVariant(uint16(DefaultRFCOMMChannel)),
    }
    pm := m.bus.Object(bluezService, dbus.ObjectPath("/org/bluez"))
    if call := pm.Call(profileManagerIface+".RegisterProfile", 0, m.serverPath, SPPUUID, optsMap); call.Err != nil {
        return fmt.Errorf("connmgr: RegisterProfile(server): %w", call.Err)
    }
    // On close, unregister server profile before closing the bus.
    m.cleanup = append(m.cleanup, func() {
        _ = pm.Call(profileManagerIface+".UnregisterProfile", 0, m.serverPath).Err
        // Unexport the object path (best-effort).
        _ = m.bus.Export(nil, m.serverPath, profileInterfaceName)
    })
    m.role = roleServer
    return nil
}

func (m *mgr) Accept(ctx context.Context) (fd int, remote Device, err error) {
    m.mu.Lock()
    if m.closed {
        m.mu.Unlock()
        return 0, Device{}, errors.New("connmgr: closed")
    }
    if m.role != roleServer || !m.serverExported {
        m.mu.Unlock()
        return 0, Device{}, errors.New("connmgr: server not started")
    }
    if m.acceptUsed {
        m.mu.Unlock()
        return 0, Device{}, errors.New("connmgr: Accept already used")
    }
    m.acceptUsed = true
    ch := m.srvProf.ch
    m.mu.Unlock()

    select {
    case <-ctx.Done():
        return 0, Device{}, fmt.Errorf("connmgr: accept canceled: %w", ctx.Err())
    case res := <-ch:
        return res.fd, res.dev, res.err
    }
}

func (m *mgr) ScanSPP(ctx context.Context) ([]Device, error) {
    m.mu.Lock()
    if m.closed {
        m.mu.Unlock()
        return nil, errors.New("connmgr: closed")
    }
    if err := m.ensureBusLocked(); err != nil {
        m.mu.Unlock()
        return nil, err
    }
    bus := m.bus
    m.mu.Unlock()

    // Discover adapters.
    adapters, err := listAdapters(bus)
    if err != nil {
        return nil, err
    }
    // Start discovery on all adapters (best-effort); stop when done.
    for _, ap := range adapters {
        _ = bus.Object(bluezService, ap).Call(adapterIface+".StartDiscovery", 0).Err
        defer func(p dbus.ObjectPath) { _ = bus.Object(bluezService, p).Call(adapterIface+".StopDiscovery", 0).Err }(ap)
    }

    // Prime from current managed objects.
    devMap, err := snapshotSPPDevices(bus)
    if err != nil {
        return nil, err
    }

    // Subscribe to InterfacesAdded to catch new devices until ctx is done.
    sigCh := make(chan *dbus.Signal, 16)
    bus.Signal(sigCh)
    defer bus.RemoveSignal(sigCh)
    if err := bus.AddMatchSignal(
        dbus.WithMatchInterface(objManagerIface),
        dbus.WithMatchMember("InterfacesAdded"),
    ); err != nil {
        return nil, fmt.Errorf("connmgr: AddMatchSignal: %w", err)
    }
    defer func() {
        _ = bus.RemoveMatchSignal(
            dbus.WithMatchInterface(objManagerIface),
            dbus.WithMatchMember("InterfacesAdded"),
        )
    }()

    loop:
    for {
        select {
        case <-ctx.Done():
            break loop
        case sig := <-sigCh:
            if sig == nil || len(sig.Body) < 2 {
                continue
            }
            path, _ := sig.Body[0].(dbus.ObjectPath)
            ifaces, _ := sig.Body[1].(map[string]map[string]dbus.Variant)
            if ifaces == nil {
                continue
            }
            if dev, ok := deviceFromIfaces(path, ifaces); ok {
                devMap[dev.Path] = dev
            }
        }
    }

    // Build stable slice.
    out := make([]Device, 0, len(devMap))
    for _, d := range devMap {
        out = append(out, d)
    }
    return out, nil
}

func (m *mgr) Connect(ctx context.Context, dev Device) (fd int, err error) {
    if dev.Path == "" {
        return 0, errors.New("connmgr: device path required")
    }
    m.mu.Lock()
    if m.closed {
        m.mu.Unlock()
        return 0, errors.New("connmgr: closed")
    }
    if m.role == roleServer || m.acceptUsed {
        m.mu.Unlock()
        return 0, errors.New("connmgr: already used as server")
    }
    if m.connectUsed {
        m.mu.Unlock()
        return 0, errors.New("connmgr: Connect already used")
    }
    if err := m.ensureBusLocked(); err != nil {
        m.mu.Unlock()
        return 0, err
    }

    // Export Profile1 for client role once.
    if !m.clientExported {
        m.cliProf = &profile{ch: make(chan acceptResult, 1)}
        // Unique client path per instance.
        id := atomic.AddUint64(&pathCounter, 1)
        m.clientPath = dbus.ObjectPath("/org/bluetooth_chat/connmgr/client/p" + strconv.FormatUint(id, 10))
        if err := m.bus.Export(m.cliProf, m.clientPath, profileInterfaceName); err != nil {
            m.mu.Unlock()
            return 0, fmt.Errorf("connmgr: export client profile: %w", err)
        }
        pm := m.bus.Object(bluezService, dbus.ObjectPath("/org/bluez"))
        optsMap := map[string]dbus.Variant{
            "Role": dbus.MakeVariant("client"),
            // Name is not used by client, but harmless to omit.
        }
        if call := pm.Call(profileManagerIface+".RegisterProfile", 0, m.clientPath, SPPUUID, optsMap); call.Err != nil {
            m.mu.Unlock()
            return 0, fmt.Errorf("connmgr: RegisterProfile(client): %w", call.Err)
        }
        // Unregister client profile on close.
        m.cleanup = append(m.cleanup, func() {
            _ = pm.Call(profileManagerIface+".UnregisterProfile", 0, m.clientPath).Err
            _ = m.bus.Export(nil, m.clientPath, profileInterfaceName)
        })
        m.clientExported = true
        m.role = roleClient
    }
    ch := m.cliProf.ch
    m.connectUsed = true
    bus := m.bus
    m.mu.Unlock()

    // Ensure paired; if not, attempt Pair() via Agent.
    devPath := dbus.ObjectPath(dev.Path)
    devObj := bus.Object(bluezService, devPath)
    var pairedVar dbus.Variant
    if call := devObj.Call(propsIface+".Get", 0, deviceIface, "Paired"); call.Err == nil {
        if err := call.Store(&pairedVar); err == nil {
            if b, ok := pairedVar.Value().(bool); ok && !b {
                if err := devObj.Call(deviceIface+".Pair", 0).Err; err != nil {
                    return 0, fmt.Errorf("connmgr: Pair: %w", err)
                }
            }
        }
    }
    // Initiate ConnectProfile on the device.
    call := devObj.Call(deviceIface+".ConnectProfile", 0, SPPUUID)
    if call.Err != nil {
        return 0, fmt.Errorf("connmgr: ConnectProfile: %w", call.Err)
    }

    select {
    case <-ctx.Done():
        return 0, fmt.Errorf("connmgr: connect canceled: %w", ctx.Err())
    case res := <-ch:
        return res.fd, res.err
    }
}

// Close is safe for concurrent and redundant calls (idempotent).
func (m *mgr) Close() error {
    m.mu.Lock()
    if m.closed {
        m.mu.Unlock()
        return nil
    }
    m.closed = true
    cleanup := m.cleanup
    // Clear to allow GC of captured resources.
    m.cleanup = nil
    m.mu.Unlock()

    // Run cleanup outside the lock in reverse order of registration.
    for i := len(cleanup) - 1; i >= 0; i-- {
        if cleanup[i] != nil {
            cleanup[i]()
        }
    }
    return nil
}

// Helpers

func listAdapters(bus *dbus.Conn) ([]dbus.ObjectPath, error) {
    obj := bus.Object(bluezService, dbus.ObjectPath("/"))
    var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
    if call := obj.Call(objManagerIface+".GetManagedObjects", 0); call.Err != nil {
        return nil, fmt.Errorf("connmgr: GetManagedObjects: %w", call.Err)
    } else if err := call.Store(&objs); err != nil {
        return nil, fmt.Errorf("connmgr: decode GetManagedObjects: %w", err)
    }
    var out []dbus.ObjectPath
    for path, ifaces := range objs {
        if _, ok := ifaces[adapterIface]; ok {
            out = append(out, path)
        }
    }
    return out, nil
}

func snapshotSPPDevices(bus *dbus.Conn) (map[string]Device, error) {
    obj := bus.Object(bluezService, dbus.ObjectPath("/"))
    var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
    if call := obj.Call(objManagerIface+".GetManagedObjects", 0); call.Err != nil {
        return nil, fmt.Errorf("connmgr: GetManagedObjects: %w", call.Err)
    } else if err := call.Store(&objs); err != nil {
        return nil, fmt.Errorf("connmgr: decode GetManagedObjects: %w", err)
    }
    out := make(map[string]Device)
    for path, ifaces := range objs {
        if dev, ok := deviceFromIfaces(path, ifaces); ok {
            out[dev.Path] = dev
        }
    }
    return out, nil
}

func deviceFromIfaces(path dbus.ObjectPath, ifaces map[string]map[string]dbus.Variant) (Device, bool) {
    props, ok := ifaces[deviceIface]
    if !ok {
        return Device{}, false
    }
    vUUIDs, ok := props["UUIDs"]
    if !ok {
        return Device{}, false
    }
    uu, _ := vUUIDs.Value().([]string)
    if !containsUUID(uu, SPPUUID) {
        return Device{}, false
    }
    var mac, name, alias string
    if v, ok := props["Address"]; ok {
        mac, _ = v.Value().(string)
    }
    if v, ok := props["Name"]; ok {
        name, _ = v.Value().(string)
    }
    if v, ok := props["Alias"]; ok {
        alias, _ = v.Value().(string)
    }
    if mac == "" {
        mac = macFromPath(path)
    }
    return Device{
        Path: string(path),
        MAC:  mac,
        Name: name,
        Alias: alias,
        // ServiceName: optional; omitted here.
    }, true
}

func containsUUID(list []string, target string) bool {
    for _, s := range list {
        if strings.EqualFold(s, target) {
            return true
        }
    }
    return false
}

func macFromPath(p dbus.ObjectPath) string {
    s := string(p)
    // Expect .../dev_XX_XX_XX_XX_XX_XX
    idx := strings.LastIndex(s, "/dev_")
    if idx < 0 {
        return ""
    }
    mac := s[idx+5:]
    mac = strings.ReplaceAll(mac, "_", ":")
    return mac
}
