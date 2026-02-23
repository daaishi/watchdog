# Watchdog - Windows Process Monitor

Windows上のアプリケーションを監視し、応答停止時に自動で再起動するツールです。
デジタルサイネージ、メディアインスタレーション、キオスク端末での利用を想定しています。

## Quick Start

1. `watchdog.exe` と `config.json` を同じフォルダに配置
2. `watchdog.exe` を実行
3. ブラウザで http://localhost:4649 を開く
4. Web UIからアプリを追加・設定

## 監視方法

| 方法 | 説明 | 用途 |
|------|------|------|
| **Process** | プロセスの生存をPIDで確認 | 一般的なアプリ |
| **Window** | 指定タイトルのウィンドウ存在を確認 | GUI アプリ |
| **UDP** | アプリからのハートビートパケットを受信 | 自作アプリ (TouchDesigner等) |
| **HTTP** | ヘルスチェックURLをポーリング | Webサーバー内蔵アプリ (Unity等) |
| **File** | ファイルのタイムスタンプ更新を確認 | ファイル出力するアプリ |

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

- `schedule` が `null` または省略 → 常時稼働（従来どおり）
- `days` が空配列 → 毎日
- 深夜跨ぎも対応（例: `"start_time": "22:00", "stop_time": "06:00"`）

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

### ビルド

```powershell
.\build.ps1
```

対話式でバージョンを選択（patch / minor / major / 変更なし）し、ビルド → zip作成まで行います。

### 必要環境

- Go 1.25+
- Windows (syscall依存)
