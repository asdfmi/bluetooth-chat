設計書: Linux間 Bluetooth SPP 1対1チャット (Go, D-Bus, /dev/rfcomm 不使用)

1. ゴール
  - BlueZ を用い、RFCOMM(Serial Port Profile; UUID: 00001101-0000-1000-8000-00805f9b34fb) で 1対1チャット。
  - サーバ: BlueZ ProfileManager1 に SPP を登録し、Profile1.NewConnection で受け取る UnixFD を直接 read/write。
  - クライアント: MAC アドレスは未知。D-Bus で発見・ペアリング・接続を行い、Profile1.NewConnection で受け取る UnixFD を直接 read/write。
  - /dev/rfcommX は登場させない（FD直扱い）。

2. モジュール構成
  A. ConnectionManager（接続制御; 低層）
     - Server:
       - D-Bus(org.bluez.ProfileManager1, Profile1) で SPP を Role="server" + 固定チャネル=22 で公開。
       - SPP サービス名（service name）を ProfileManager1.RegisterProfile の options["Name"] に設定。
         （この値はリモート側のSDPで参照可能。クライアントは一覧表示に利用。）
       - 接続成立時に Profile1.NewConnection(fd) を受領し、fd を上位(B)へ引き渡す。
     - Client:
       - D-Bus で周辺デバイスを発見(Adapter1.StartDiscovery + ObjectManager/InterfacesAdded)し、Device1.UUIDs に SPP UUID を含むものを抽出。
       - 必要に Device1.Pair()、続けて Device1.ConnectProfile(SPP_UUID) を呼ぶ。
       - スキャン時のデバイス一覧は「SPPサービス名（SDPのServiceName属性; サーバの RegisterProfile options["Name"]）を優先表示」。
         取得できない場合は Device1.Alias/Name を代替表示。併せて MAC も表示。
         ユーザが手動で選択（choose）したデバイスに対してのみ Pair/Connect を実施。
       - 自プロセスにも Profile1 を Role="client" で登録し、接続成立時の Profile1.NewConnection(fd) を受領して上位(B)へ渡す。
     - 責務: 「FDを準備して渡す」まで。再接続は本MVPでは行わない。
  B. Transport（バイトストリームI/O）
     - 標準パッケージ（os/io）のみで、受け取った FD を os.NewFile で *os.File にラップし Read/Write/Close を提供。I/O は goroutine + ブロッキングで実装。
  C. Framing（メッセージ化）
     - LF改行区切りで bytes⇆string。
  D. CLI/App（UI最小）
     - 引数: `-role (server|client)`, `-name <service-name>`。
       - server: `-name` は必須（SPPサービス名）。RegisterProfile options["Name"] に設定。
       - client: `-name` は使用しない（自動選択は行わない）。
     - クライアントは「scan → 一覧表示 → ユーザが番号で choose → 接続」というフローのみを提供。
     - スキャン一覧は「SPPサービス名（SDP; RegisterProfile options["Name"]）優先 + MAC」を列挙（取得不能時は Alias/Name）。
     - stdin 行→C.Encode→B.Write、B.Read→C.Feed→表示。

3. 依存/使用API
  - dbus: github.com/godbus/dbus/v5

4. データパス（送信時）
  App.Write → Transport.Write(*os.File.Write) → カーネル RFCOMM(フレーミング/クレジット制御)
             → L2CAP(多重化/分割再結合) → HCI(ACLデータ化) → BTコントローラ → 無線
  受信は逆順。D-Bus は制御面のみ（FD受け渡し）でデータ面には登場しない。

5. シーケンス概要
  5.1 Server
    main → A.StartListen(serviceName)
      ├─ D-Bus: Profile1 を自プロセスに Export(Role="server")
      ├─ D-Bus: ProfileManager1.RegisterProfile(obj, SPP_UUID, {"Role":"server","Channel":22,"Name":serviceName})
      └─ 接続待機 → D-Bus: Profile1.NewConnection(dev, fd, props)
           ├─ A: fd を受領 → B.OpenFromFD(fd)
           └─ D: goroutineで B.Read ループ開始
  5.2 Client
    main → A.ScanSPP(SPP_UUID) → 一覧表示（SPPサービス名優先） → ユーザが1台を選択
      ├─ D-Bus: Adapter1.StartDiscovery()
      ├─ D-Bus: InterfacesAdded を購読し、Device1.UUIDs に SPP_UUID を含むデバイスを列挙
      ├─ （表示用）各デバイスに対し Device1.DiscoverServices(SPP_UUID) を行い、SDP ServiceName(0x0100) を取得
      ├─ 取得できない場合は Device1.Alias/Name を表示名として利用。MAC も併記
      ├─ ユーザの選択結果（devicePath）を取得
      ├─ D-Bus: （未ペア時のみ）Device1.Pair()
      ├─ D-Bus: Device1.ConnectProfile(SPP_UUID)
      ├─ D-Bus: 自プロセス側の Profile1(Role="client") に NewConnection(fd) が飛ぶ
      └─ A: fd 受領 → B.OpenFromFD(fd)

6. 代表エラーハンドリング（MVP最小）
  - B.Write: 書き込みエラー（相手切断等）→ 終了。
  - B.Read: n==0/エラー（EOF/切断）→ 終了。
  - A.RegisterProfile / NewConnection: D-Bus エラーは即時失敗として上位へ返す。
  - A.DiscoverAndConnect:
    - Discovery タイムアウト → エラー返却。
    - Pair 失敗/拒否 → エラー返却。
    - ConnectProfile 失敗 → エラー返却。

7. ビルド/実行例（要点）
  - 前提: Linux, BlueZ 稼働, System bus へアクセス可（必要なら root）。
  - サーバ: ./chat -role=server -name="MyChatService"
  - クライアント: ./chat -role=client  （起動後、一覧から番号を選択して接続）
  - 双方で標準入力に文字を打てば相互表示。

8. 要点
  - クライアントは MAC 未知。Discovery + ConnectProfile + Profile1.NewConnection で FD を取得。
  - サーバ/クライアントとも /dev/rfcommX は使わず、**FD直I/O**。
  - A が FD を用意し、B が FD を I/O、C/D は上位ロジックのみ。
  - サーバは `-name`（SPPサービス名）必須。RegisterProfile options["Name"] に設定し、クライアントのスキャン一覧で表示名として利用。
  - クライアントは scan→choose のみ。自動選択や名前フィルタは行わない。
  - RFCOMMチャネルは 22 に固定（サーバの RegisterProfile options["Channel"] に設定）。

9. 動作確認（モジュール別）
  9.1 A: ConnectionManager（接続制御; D-Bus）
    Server 側:
      手順:
        - BlueZ/System bus へアクセス可能なユーザで起動: `./chat -role=server -name="MyChatService"`。
        - SDP登録確認: 同一ホストまたは別ホストから `sdptool browse <server-mac|local>` を実行し、SPP(Serial Port)エントリの Service Name と RFCOMM Channel を確認。
        - 接続確認: 別ホストのクライアントから接続（9.4 参照）。同時に `dbus-monitor --system "type='method_call',interface='org.bluez.Profile1',member='NewConnection'"` を実行し、呼び出し到来を確認。
      期待結果:
        - RegisterProfile が成功し、SDPに「Service Name: MyChatService」「Channel: 22」が表示される。
        - クライアント接続時に Profile1.NewConnection が呼ばれ、UnixFD を1本受領できる。
        - 切断時、以降の Read は EOF、Write はエラーとなる。

    Client 側:
      手順:
        - 起動: `./chat -role=client`。
        - 発見確認: `dbus-monitor --system "type='signal',interface='org.freedesktop.DBus.ObjectManager',member='InterfacesAdded'"` を併用し、SPP UUID を含むデバイスが検出されることを確認。
        - 一覧から1台を選択。未ペアの場合は Pair プロンプト（OS/UI）が表示される場合があるので承認。
        - 接続成立時、自プロセスの Profile1(Role="client") に対して `NewConnection` が飛ぶことを `dbus-monitor` で確認。
      期待結果:
        - 一覧には SPP(UUID: 00001101-0000-1000-8000-00805f9b34fb) を持つデバイスのみが並ぶ。
        - 表示名は可能なら SDPのServiceName（例: MyChatService）を使用。取得不能時は Alias/Name。MAC も併記。
        - 選択後、未ペアなら Pair が成功し、その後 ConnectProfile が成功。続いて自プロセスの Profile1 に NewConnection が到来し、UnixFD を受領する。
        - Discovery は選択操作で停止。タイムアウト時は明確なエラーで終了。

  9.2 B: Transport（バイトストリームI/O）
    手順（単体）:
      - （x/sys/unix 非使用のため）ユニット検証は以下のいずれかで代替:
        - `net.Pipe()` を用いて Read/Write/Close の往復挙動を確認（FD 不要）。
        - もしくは `os.Pipe()` を2本用いて相互接続し、片側クローズ時に B.Read が EOF、B.Write がエラーとなることを確認。
      手順（結合: A+B）:
      - A 経由で FD を取得（サーバ/クライアント接続完了後）。通常どおり標準入力から送受信し、B が問題なく I/O できることを確認。
    期待結果:
      - Write は送信バイト数を返し、Read は到着バイトをそのまま返す（改行等の加工なし）。
      - ピア Close で Read は 0/EOF、以降の Write はエラーとなり、アプリは終了。

  9.3 C: Framing（メッセージ化）
    手順:
      - Encode: 入力 "abc" → 出力 "abc\n"。空文字列 "" → "\n"。
      - Decode: 入力ストリーム "a\nb\n\nc" を順に Feed し、"a", "b", "" の3メッセージを取り出せること。末尾の "c" は次の LF が来るまで保留されること。
      - 改行系: 送信は LF のみ付与。受信は LF 区切りを前提（CRLF は MVP では未サポート扱い。必要なら上位で CR 除去）。
    期待結果:
      - エンコードは常に末尾に LF を1つだけ追加し、内部でエスケープは行わない。
      - デコードは LF を区切りとして分割。部分行は次の入力まで保持し、LF を含む長文も問題なく取り扱える。

  9.4 D: CLI/App（UI最小）
    手順（E2E）:
      - 構成: 2台の Linux もしくは 1台 + USB BT ドングルでアダプタを分ける。
      - サーバ: `./chat -role=server -name="MyChatService"` を実行。
      - クライアント: `./chat -role=client` を実行し、一覧から "MyChatService <MAC>" を選択。
      - 接続後、双方端末で1行ずつ文字列を入力し、相手側に即時表示されることを確認。
      - 切断試験: サーバプロセス終了 or BT オフで、クライアント側が EOF/エラーを検知して終了すること（逆方向も同様）。
    手順（異常系）:
      - サーバで `-name` 未指定 → 起動失敗（usage/エラー出力）。
      - クライアントで選択デバイスが SPP 未提供/到達不可 → ConnectProfile 失敗を表示して終了。
      - ペアリング拒否 → 明確なエラー表示で終了。
    期待結果:
      - サーバは `-name` 必須チェックにより、未指定時は非0終了。
      - クライアントは自動接続/自動選択を行わず、ユーザ選択時のみ接続。成功時に FD を受領し I/O 開始。
      - 同時接続は MVP では非対応。既に接続中に2本目が到来した場合、2本目の FD は即時 Close（または拒否）し、既存接続を維持。
      - 終了時にプロファイル登録解除（UnregisterProfile）および `Profile1.Release` を適切に処理。
