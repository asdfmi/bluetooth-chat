Design Document: Bluetooth SPP 1-to-1 Chat Between Linux Hosts (Go, D-Bus, No /dev/rfcomm)

1. Goal
  - Implement a 1-to-1 chat using BlueZ and RFCOMM (Serial Port Profile; UUID: 00001101-0000-1000-8000-00805f9b34fb).
  - Server: Register SPP via BlueZ ProfileManager1, handle UnixFD from Profile1.NewConnection directly with read/write.
  - Client: MAC address is unknown. Discover, pair, and connect via D-Bus, handle UnixFD from Profile1.NewConnection directly with read/write.
  - /dev/rfcommX will not be used (FD handled directly).

2. Module Structure
  A. ConnectionManager (Connection Control; Low Layer)
     - Server:
       - Expose SPP via D-Bus (org.bluez.ProfileManager1, Profile1) as Role="server" + fixed channel=22.
       - Set SPP service name in options["Name"] of ProfileManager1.RegisterProfile.
         (This value is referenced in the remote SDP and listed by clients.)
       - Upon connection, receive Profile1.NewConnection(fd) and pass fd to upper layer (B).
     - Client:
       - Discover nearby devices via D-Bus (Adapter1.StartDiscovery + ObjectManager/InterfacesAdded), filter those whose Device1.UUIDs include SPP UUID.
       - Call Device1.Pair() if needed, then Device1.ConnectProfile(SPP_UUID).
       - During scan, prioritize displaying the SPP service name (SDP ServiceName; server’s RegisterProfile options["Name"]).
         If unavailable, use Device1.Alias/Name as fallback. Display MAC alongside.
         Only the user-selected device is paired/connected.
       - Register Profile1 with Role="client" to receive Profile1.NewConnection(fd) and pass fd to upper layer (B).
     - Responsibility: Prepare and hand over FD. Reconnection is not implemented in this MVP.
  B. Transport (Byte Stream I/O)
     - Use only standard packages (os/io). Wrap received FD via os.NewFile into *os.File, providing Read/Write/Close.
       I/O implemented with goroutines and blocking operations.
  C. Framing (Message Handling)
     - LF-delimited conversion between bytes and string.
  D. CLI/App (Minimal UI)
     - Args: `-role (server|client)`, `-name <service-name>`.
       - server: `-name` is mandatory (SPP service name), set to RegisterProfile options["Name"].
       - client: `-name` unused (no auto-selection).
     - Client flow: “scan → list → user choose → connect”.
     - Scan list shows “SPP service name (SDP; RegisterProfile options["Name"]) + MAC” (fallback to Alias/Name if unavailable).
     - stdin line → C.Encode → B.Write, B.Read → C.Feed → display.

3. Dependencies / APIs
  - dbus: github.com/godbus/dbus/v5

4. Data Path (Transmit)
  App.Write → Transport.Write(*os.File.Write) → Kernel RFCOMM (framing/credit control)
             → L2CAP (multiplexing/reassembly) → HCI (ACL encapsulation) → BT Controller → Air
  Reception is the reverse. D-Bus is only used for control (FD passing), not for data transfer.

5. Sequence Overview
  5.1 Server
    main → A.StartListen(serviceName)
      ├─ D-Bus: Export Profile1 (Role="server")
      ├─ D-Bus: ProfileManager1.RegisterProfile(obj, SPP_UUID, {"Role":"server","Channel":22,"Name":serviceName})
      └─ Wait for connection → D-Bus: Profile1.NewConnection(dev, fd, props)
           ├─ A: receive fd → B.OpenFromFD(fd)
           └─ D: start goroutine for B.Read loop
  5.2 Client
    main → A.ScanSPP(SPP_UUID) → display list (SPP service name prioritized) → user selects device
      ├─ D-Bus: Adapter1.StartDiscovery()
      ├─ D-Bus: Listen for InterfacesAdded, list devices with SPP_UUID in Device1.UUIDs
      ├─ For display: call Device1.DiscoverServices(SPP_UUID) to get SDP ServiceName (0x0100)
      ├─ If unavailable, use Device1.Alias/Name as display name, append MAC
      ├─ Get selected devicePath
      ├─ D-Bus: Device1.Pair() (if not paired)
      ├─ D-Bus: Device1.ConnectProfile(SPP_UUID)
      ├─ D-Bus: Receive Profile1.NewConnection(fd) on client Profile1(Role="client")
      └─ A: receive fd → B.OpenFromFD(fd)

6. Representative Error Handling (MVP Minimal)
  - B.Write: On write error (peer disconnect, etc.) → exit.
  - B.Read: n==0 or error (EOF/disconnect) → exit.
  - A.RegisterProfile / NewConnection: D-Bus error → fail immediately and return to upper layer.
  - A.DiscoverAndConnect:
    - Discovery timeout → return error.
    - Pair failure/rejection → return error.
    - ConnectProfile failure → return error.

7. Build / Run Examples (Key Points)
  - Requirements: Linux, BlueZ running, access to system bus (root if needed).
  - Server: ./chat -role=server -name="MyChatService"
  - Client: ./chat -role=client  (after launch, select device from list to connect)
  - Typing text on either terminal shows it on the other.

8. Key Points
  - Client’s MAC is unknown. Use Discovery + ConnectProfile + Profile1.NewConnection to get FD.
  - Both server and client avoid /dev/rfcommX; **direct FD I/O** is used.
  - A prepares FD, B handles I/O, C/D handle higher-level logic.
  - Server requires `-name` (SPP service name). Set to RegisterProfile options["Name"], used as display name by client.
  - Client only performs scan→choose; no auto-selection or name filtering.
  - RFCOMM channel fixed to 22 (set in server’s RegisterProfile options["Channel"]).

9. Verification (By Module)
  9.1 A: ConnectionManager (Connection Control; D-Bus)
    Server Side:
      Steps:
        - Run as user with BlueZ/system bus access: `./chat -role=server -name="MyChatService"`.
        - Verify SDP registration: run `sdptool browse <server-mac|local>` and confirm “Service Name: MyChatService” and “Channel: 22”.
        - Verify connection: connect from client (see 9.4) and monitor `dbus-monitor --system "type='method_call',interface='org.bluez.Profile1',member='NewConnection'"`.
      Expected:
        - RegisterProfile succeeds; SDP shows “Service Name: MyChatService”, “Channel: 22”.
        - On client connect, Profile1.NewConnection is invoked, one UnixFD received.
        - After disconnect, subsequent Read gives EOF, Write returns error.

    Client Side:
      Steps:
        - Run: `./chat -role=client`.
        - Verify discovery: monitor `dbus-monitor --system "type='signal',interface='org.freedesktop.DBus.ObjectManager',member='InterfacesAdded'"`, confirm detection of devices with SPP UUID.
        - Choose one from list. If unpaired, pairing prompt (OS/UI) may appear; approve it.
        - On connection success, verify `Profile1.NewConnection` invoked on client’s Profile1(Role="client").
      Expected:
        - List shows only devices with SPP (UUID: 00001101-0000-1000-8000-00805f9b34fb).
        - Display name is SDP ServiceName (e.g. MyChatService) if available; otherwise Alias/Name. MAC always shown.
        - After selection, Pair succeeds (if needed), ConnectProfile succeeds, and client’s Profile1 receives NewConnection with UnixFD.
        - Discovery stops after selection. Timeout triggers clear error and exit.

  9.2 B: Transport (Byte Stream I/O)
    Unit Test:
      - (Without x/sys/unix) verify using:
        - `net.Pipe()` to test Read/Write/Close behavior.
        - Or connect two `os.Pipe()` pairs and verify EOF on close, Write returns error after peer close.
      Integration (A+B):
      - Obtain FD from A (after connection). Verify normal I/O between peers via stdin.
    Expected:
      - Write returns number of bytes sent, Read returns received bytes unchanged.
      - When peer closes, Read gives 0/EOF, subsequent Write errors, app exits.

  9.3 C: Framing (Message Handling)
    Steps:
      - Encode: input "abc" → output "abc\n"; empty string "" → "\n".
      - Decode: input stream "a\nb\n\nc" → messages "a", "b", ""; trailing "c" kept until next LF.
      - Newline: send LF only; receive expects LF delimiter (CRLF unsupported in MVP; strip CR if needed at upper layer).
    Expected:
      - Encoder always appends exactly one LF; no escaping.
      - Decoder splits by LF, retains partial lines until next input, handles long lines with LF correctly.

  9.4 D: CLI/App (Minimal UI)
    Steps (E2E):
      - Setup: two Linux hosts or single host + USB BT dongle.
      - Server: `./chat -role=server -name="MyChatService"`.
      - Client: `./chat -role=client`, select "MyChatService <MAC>".
      - After connection, both sides can type and immediately see the other’s messages.
      - Disconnect test: terminate server or turn off BT, client detects EOF/error and exits (and vice versa).
    Abnormal Cases:
      - Server without `-name` → startup fails (usage/error message).
      - Client connecting to device without/invalid SPP → ConnectProfile fails and exits.
      - Pairing rejected → explicit error message and exit.
    Expected:
      - Server enforces mandatory `-name` and exits non-zero if missing.
      - Client never auto-connects/selects; only user-selected device connects; upon success, FD received and I/O begins.
      - No multi-connection support in MVP. If second FD arrives, close/reject it and keep existing connection.
      - On exit, properly unregister profile (UnregisterProfile) and handle `Profile1.Release`.
