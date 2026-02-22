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

## ファイル構成

```
watchdog.exe    ... 実行ファイル（これを起動）
config.json     ... 設定ファイル（Web UIで自動保存）
```

## Windows起動時に自動実行

1. `Win + R` → `shell:startup` と入力 → Enter
2. 開いたフォルダに `watchdog.exe` のショートカットを作成

## ポート番号

デフォルトは **4649** です。変更するには `config.json` の `web_port` を編集してWatchdogを再起動してください。
