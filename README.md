# Watchdog - Windows Process Monitor

Windows上のアプリケーションを監視し、応答停止時に自動で再起動するツールです。
デジタルサイネージ、メディアインスタレーション、キオスク端末での利用を想定しています。

指定時間帯での自動起動/停止、停止時の「保存してから終了」、特定時刻のネットワークコマンド送信（UDP / OSC / PJLINK）にも対応します。

## Quick Start

1. `watchdog.exe` と `config.json` を同じフォルダに配置
2. `watchdog.exe` を実行
3. ブラウザで http://localhost:4649 を開く
4. Web UIからアプリを追加・設定（右上のトグルで日本語/英語を切替できます）

## 監視方法

| 方法 | 説明 | 用途 |
|------|------|------|
| **Process** | プロセスの生存をPIDで確認 | 一般的なアプリ |
| **Window** | 指定タイトルのウィンドウ存在を確認 | GUI アプリ |
| **UDP** | アプリからのハートビートパケットを受信 | 自作アプリ (TouchDesigner等) |
| **HTTP** | ヘルスチェックURLをポーリング | Webサーバー内蔵アプリ (Unity等) |
| **File** | ファイルのタイムスタンプ更新を確認 | ファイル出力するアプリ |

応答が `timeout_sec` 秒途絶えると、プロセスを終了して再起動します。

## 設定

すべての設定は Web UI (http://localhost:4649) から行えます。
設定は `config.json` に自動保存されます。
`config.json` を直接編集することも可能です（Watchdog停止中に行ってください）。

### グローバル設定

| キー | 型 | デフォルト | 説明 |
|------|-----|-----------|------|
| `web_port` | int | `4649` | Web UIのポート番号 |
| `show_console` | bool | `false` | `true`: コンソールウィンドウを表示 / `false`: 非表示 |
| `log_dir` | string | `"logs"` | ログ出力先ディレクトリ（相対パスまたは絶対パス） |
| `reboot_time` | string | `""` | PC再起動時刻 (HH:MM形式)。空なら無効 |
| `reboot_days` | string[] | `[]` | 再起動する曜日 (例: `["Monday","Friday"]`)。空なら毎日 |

### アプリの起動方法（シェル起動）

- `use_shell_open: false`（既定）… `exe_path` を**プログラムとして直接起動**。PIDを即時・確実に取得。`.exe` 向き。
- `use_shell_open: true` … Windowsの**関連付けで開く**（ダブルクリック相当）。`.toe`（TouchDesigner）・`.mad`（MadMapper）など、直接実行できないドキュメントを開く場合に使用。

Web UIで実行ファイル欄に `.exe` 以外を選ぶと、自動的にシェル起動が有効になります。

### 停止方法（stop_mode）

スケジュール停止・手動停止時の終了のしかたを選べます。

| 値 | 説明 |
|------|------|
| `force`（既定） | 即時終了 (taskkill) |
| `graceful` | ウィンドウを前面化して **Ctrl+S（保存）→ 終了**。保存実証済みの確実な方法（前面化を確認できた時のみキー送信し、誤爆を防止） |
| `osc` | OSCで保存（＋任意で終了）コマンドを送信してから終了。`osc_host` / `osc_port` / `osc_save_addr` / `osc_quit_addr` を設定 |

※クラッシュ/フリーズ検知による再起動時は、復旧を優先するため常に強制終了します。

### アプリごとのスケジュール設定

各アプリに `schedule` を設定すると、指定時間帯のみ起動します。

```json
{
  "schedule": {
    "start_time": "09:00",
    "stop_time": "18:00",
    "days": ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday"]
  }
}
```

- `schedule` が `null` または省略 → 常時稼働
- `days` が空配列 → 毎日
- 深夜跨ぎも対応（例: `"start_time": "22:00", "stop_time": "06:00"`）
- 稼働時間帯の途中でWatchdogを起動した場合は、その場でアプリを起動します。

### スケジュール送信コマンド（UDP / OSC / PJLINK）

指定時刻・曜日にネットワークコマンドを自動送信できます（例: プロジェクターの電源ON/OFF）。
各コマンドには **テスト発火ボタン** があり、保存前/保存後に手動で送信して確認できます。

| 種別 | 送信内容 |
|------|---------|
| **UDP** | 任意のテキスト、またはHEXバイト列 |
| **OSC** | アドレス + 引数（スペース区切り、int/float/文字列を自動判定）。UDP送信 |
| **PJLINK** | TCP(4352)でグリーティング→（パスワードありならMD5認証）→クラス1コマンド（例 `POWR 1` / `POWR 0`）を送信し応答を取得 |

`time` を空にすると手動（テスト発火）専用になります。

### ファイルログ

ログは `log_dir` で指定したディレクトリに日付ごとに出力されます。

```
logs/watchdog-2024-01-15.log
logs/watchdog-2024-01-16.log
```

- `show_console: true` → コンソール + ファイル両方に出力
- `show_console: false` → ファイルのみ

## ファイル構成

```
watchdog.exe    ... 実行ファイル（これを起動）
config.json     ... 設定ファイル（Web UIで自動保存）
logs/           ... ログファイル（自動作成）
```

## Windows起動時に自動実行

1. `Win + R` → `shell:startup` と入力 → Enter
2. 開いたフォルダに `watchdog.exe` のショートカットを作成

## ポート番号

デフォルトは **4649** です。変更するには `config.json` の `web_port` を編集してWatchdogを再起動してください。

## 開発

### ビルド / 配布 / リリース

[`just`](https://github.com/casey/just) で実行します。

```powershell
just build         # dist/ に watchdog.exe をビルド（VERSIONを埋め込み）
just package       # ビルド + フラットzip作成 (watchdog-vX.Y.Z.zip)
just release       # package + git push + GitHubリリース作成 (gh)
just bump minor    # バージョン更新 (patch | minor | major) + コミット
just run           # ローカル実行（開発用）
just clean         # 成果物 (dist/, zip) を削除
```

配布パッケージには実機固有の設定を含まない `config.dist.json` が `config.json` として同梱されます。

### 必要環境

- Go 1.25+
- Windows（**Windows 10 IoT Enterprise** / Windows 11）
- ビルド/リリース用: `just`、PowerShell 7 (`pwsh`)、`gh`（GitHub CLI）
