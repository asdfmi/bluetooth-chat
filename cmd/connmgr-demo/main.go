//go:build linux
//
// Demo CLI for connmgr (Linux only)
//
// Prerequisites
// - Linux with BlueZ (bluetoothd) running and system D‑Bus access.
// - Adapter powered on: `bluetoothctl power on`.
// - Most environments require sudo for RegisterProfile: run with `sudo` if needed.
// - Initialize module (once) if not already:
//     go mod init bluetooth-chat
//     go get github.com/godbus/dbus/v5
//   (If your module name differs, update the import path below accordingly.)
//
// Modes for step-by-step verification
// 1) StartServer only (recommended first):
//     go run ./cmd/connmgr-demo -mode=start -name MyChatService -timeout=60s
//   Verify in another terminal:
//     sdptool browse local          (see Serial Port: Service Name=MyChatService, Channel=22)
//     dbus-monitor --system "type='method_call',interface='org.bluez.ProfileManager1',member='RegisterProfile'"
//
// 2) Accept one connection (server):
//     sudo go run ./cmd/connmgr-demo -mode=server -name MyChatService -timeout=120s
//   Then connect from another device (client) to SPP service "MyChatService"; observe:
//     dbus-monitor --system "type='method_call',interface='org.bluez.Profile1',member='NewConnection'"
//   The CLI prints the accepted FD and peer info.
//
// 3) Scan for SPP devices:
//     go run ./cmd/connmgr-demo -mode=scan -timeout=15s
//   Lists devices with Path/MAC/Name/Alias (Path is always non-empty).
//
// 4) Connect to a device (client):
//   a) Interactive (scan then choose):
//       sudo go run ./cmd/connmgr-demo -mode=connect -timeout=120s
//   b) Direct by object path:
//       sudo go run ./cmd/connmgr-demo -mode=connect -device /org/bluez/hci0/dev_XX_XX_XX_XX_XX_XX -timeout=120s
//   If not paired, an Agent must be registered; pairing is attempted automatically.
//
// Notes
// - Exit/Ctrl‑C cancels via context.
// - The printed FD is owned by the caller; wrap with os.NewFile and close yourself.
// - WSL is generally unsupported unless you pass through a USB BT adapter and run bluetoothd in WSL2.
//
package main

import (
    "bufio"
    "context"
    "flag"
    "fmt"
    "log"
    "os"
    "os/signal"
    "strconv"
    "strings"
    "syscall"
    "time"

    "bluetooth-chat/internal/connmgr"
)

func main() {
    mode := flag.String("mode", "scan", "mode: scan|start|server|connect")
    name := flag.String("name", "MyChatService", "SPP service name (server mode)")
    devPath := flag.String("device", "", "Device object path to connect (connect mode). If empty, scan and prompt.")
    timeout := flag.Duration("timeout", 15*time.Second, "operation timeout")
    flag.Parse()

    // Context with timeout + Ctrl-C cancellation
    ctx, cancel := context.WithTimeout(context.Background(), *timeout)
    defer cancel()
    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
    go func() {
        <-sig
        cancel()
    }()

    m := connmgr.New()
    defer func() {
        if err := m.Close(); err != nil {
            log.Printf("close error: %v", err)
        }
    }()

    switch strings.ToLower(*mode) {
    case "scan":
        runScan(ctx, m)
    case "start", "startserver":
        runStartServer(ctx, m, *name)
    case "server":
        runServer(ctx, m, *name)
    case "connect":
        runConnect(ctx, m, *devPath)
    default:
        log.Fatalf("unknown mode: %s", *mode)
    }
}

func runScan(ctx context.Context, m connmgr.Mgr) {
    devs, err := m.ScanSPP(ctx)
    if err != nil {
        log.Fatalf("ScanSPP error: %v", err)
    }
    if len(devs) == 0 {
        fmt.Println("no SPP devices found")
        return
    }
    for i, d := range devs {
        fmt.Printf("[%d] Path=%s MAC=%s Name=%s Alias=%s\n", i, d.Path, d.MAC, d.Name, d.Alias)
    }
}

func runStartServer(ctx context.Context, m connmgr.Mgr, serviceName string) {
    if serviceName == "" {
        log.Fatal("-name is required in start mode")
    }
    if err := m.StartServer(ctx, connmgr.ServerOptions{ServiceName: serviceName}); err != nil {
        log.Fatalf("StartServer error: %v", err)
    }
    log.Printf("SPP server registered: Name=%s Channel=22", serviceName)
    log.Printf("Now waiting (no Accept). Use sdptool/dbus-monitor to verify. Timeout=%s", deadlineStr(ctx))
    <-ctx.Done()
    if ctx.Err() != nil {
        log.Printf("context done: %v", ctx.Err())
    }
}

func runServer(ctx context.Context, m connmgr.Mgr, serviceName string) {
    if serviceName == "" {
        log.Fatal("-name is required in server mode")
    }
    if err := m.StartServer(ctx, connmgr.ServerOptions{ServiceName: serviceName}); err != nil {
        log.Fatalf("StartServer error: %v", err)
    }
    log.Printf("SPP server started: Name=%s Channel=22", serviceName)
    log.Printf("Waiting for incoming connection (timeout=%s)...", deadlineStr(ctx))
    fd, peer, err := m.Accept(ctx)
    if err != nil {
        log.Fatalf("Accept error: %v", err)
    }
    fmt.Printf("ACCEPTED: fd=%d peer.Path=%s peer.MAC=%s peer.Name=%s peer.Alias=%s\n", fd, peer.Path, peer.MAC, peer.Name, peer.Alias)
}

func runConnect(ctx context.Context, m connmgr.Mgr, path string) {
    var dev connmgr.Device
    if path == "" {
        // Scan and interactively choose
        fmt.Println("Scanning for SPP devices to choose...")
        devs, err := m.ScanSPP(ctx)
        if err != nil {
            log.Fatalf("ScanSPP error: %v", err)
        }
        if len(devs) == 0 {
            fmt.Println("no SPP devices found")
            return
        }
        for i, d := range devs {
            fmt.Printf("[%d] Path=%s MAC=%s Name=%s Alias=%s\n", i, d.Path, d.MAC, d.Name, d.Alias)
        }
        fmt.Print("Choose index: ")
        idx := readIndex(len(devs))
        dev = devs[idx]
    } else {
        dev = connmgr.Device{Path: path}
    }
    log.Printf("Connecting to %s (timeout=%s)...", dev.Path, deadlineStr(ctx))
    fd, err := m.Connect(ctx, dev)
    if err != nil {
        log.Fatalf("Connect error: %v", err)
    }
    fmt.Printf("CONNECTED: fd=%d dev.Path=%s\n", fd, dev.Path)
}

func readIndex(n int) int {
    r := bufio.NewReader(os.Stdin)
    for {
        line, _ := r.ReadString('\n')
        line = strings.TrimSpace(line)
        i, err := strconv.Atoi(line)
        if err == nil && i >= 0 && i < n {
            return i
        }
        fmt.Printf("enter 0..%d: ", n-1)
    }
}

func deadlineStr(ctx context.Context) string {
    if d, ok := ctx.Deadline(); ok {
        return time.Until(d).Truncate(time.Second).String()
    }
    return "none"
}
