# tailagent 要件定義書

- バージョン: 0.1
- 対象: MVP
- 対応環境: ローカルPC（初期はmacOS）
- 実装方針: Go
- 保存先: SQLite
- UI: シンプルなローカルWeb UIまたはGoデスクトップUI
- 観測性: 内蔵Trace UIを標準とし、OpenTelemetry連携は任意

---

## 1. 概要

tailagentは、Codex・Claude・KiroなどのAIコーディングエージェントを、ローカル環境で一元管理するためのオーケストレーションツールである。

ユーザーはtailagent上で以下を行う。

- ローカルAgentの登録
- 既存ローカルフォルダのProject登録
- Milestone・Taskの管理
- TaskからAgentへの指示
- Agent実行状況の確認
- Agentが要求したPermissionのAllow / Deny
- ログ・エラー・Approval・実行履歴のTrace確認

主目的は、Agentごとのターミナルを行き来するスイッチコストを減らし、複数Agentの仕事・承認・履歴を一つの画面へまとめることである。

---

## 2. ゴール

### 2.1 プロダクトゴール

1. Codex、Claude、Kiroを一つのローカルUIから管理できる
2. Project・Milestone・Task単位で仕事を整理できる
3. TaskからAgentへ指示を送信できる
4. Permission要求をtailagent上で処理できる
5. 実行ログ、Tool Call、Approval、失敗理由を追跡できる
6. 外部クラウドを必須にせず運用できる
7. UIをGoで無理なく実装できる程度に単純化する

### 2.2 MVP成功条件

- Agentを追加できる
- ローカルフォルダをProjectとして登録できる
- Project配下にTaskを作成できる
- Milestone未指定でもTaskを作成できる
- TaskをAgentへ割り当てて実行できる
- stdout / stderrを取得できる
- Approval要求を一覧表示できる
- Allow / Deny結果をAgentへ返せる
- Trace画面で実行履歴を確認できる
- 主要データをSQLiteに保存できる

---

## 3. 対象ユーザー

- 複数のAIコーディングエージェントを使う開発者
- Codex、Claude、Kiroを並行利用するユーザー
- Agentの動作をローカルで監視したいユーザー
- Permission承認のために複数ターミナルへ戻ることを避けたいユーザー

---

## 4. 情報構造

```text
Agent

Project
├── Milestone（任意）
│   └── Task
└── None Milestone（自動作成）
    └── Milestone未指定のTask

Task
└── Agent Run
    ├── Trace Event
    ├── Log
    └── Approval
```

### 4.1 Agent

実際に作業するAIコーディングエージェント。

対象:

- Codex
- Claude
- Kiro

### 4.2 Project

既にローカルに存在するフォルダをtailagentへインポートしたもの。

### 4.3 Milestone

Taskを任意にグループ化する単位。必須ではない。

### 4.4 None Milestone

Project作成時に自動作成される内部的なデフォルトMilestone。

Task作成時にMilestoneを指定しなかった場合、そのTaskはNone Milestoneへ紐づく。

要件:

- Projectごとに1件だけ存在
- 削除不可
- 名前変更不可
- 通常Milestoneと区別するフラグを持つ
- UIでは `None` または `No milestone` と表示
- 後から通常Milestoneへ変更可能

### 4.5 Task

Agentへ依頼する作業単位。必ずProjectに属する。

### 4.6 Approval

Agentがコマンド実行、ファイルアクセス、ネットワークアクセス等の許可を求めた際の承認要求。

### 4.7 Trace

Agent Run、Tool Call、Approval、ログ、失敗、結果を時系列で記録したもの。

---

## 5. 画面構成

左サイドバー:

1. Agents
2. Projects
3. Milestones
4. Tasks
5. Approvals
6. Traces
7. Settings

### 5.1 共通ヘッダ

各画面に以下を表示する。

- 画面名
- 短い説明
- 右上の画面固有アクション

`local`表示は不要。ローカル動作を前提とする。

### 5.2 右詳細ペイン

一覧・カード・行の詳細は右側のオーバーレイペインへ表示する。

要件:

- 背面の一覧やカンバンの幅を縮めない
- 背面レイアウトを変更しない
- 画面右側へ重ねて表示
- 幅は約500〜560px
- `×`で閉じられる
- `Esc`で閉じられる
- 背面の空白クリックで閉じられる
- 詳細表示中に同じ行をクリックすると閉じる
- 別の行をクリックすると、その項目へ切り替える
- 閲覧モードと編集モードを分ける
- `Edit`で編集モード
- `Save`で保存して閲覧モードへ戻る

### 5.3 Go実装を考慮したUI制約

MVPでは以下を避ける。

- 複雑なアニメーション
- 多段モーダル
- 高度なリッチテキスト編集
- 複雑なドラッグ操作
- 多層タブ
- 大量のネストメニュー

基本UI:

- テーブル
- カンバン
- セレクト
- 入力フォーム
- 右詳細ペイン
- ボタン
- 単純な折れ線グラフ

---

## 6. Agents

### 6.1 初期状態

Agentは0件。

中央に以下を表示する。

- 空状態アイコン
- `No agents configured`
- `+ Agent`

ヘッダ右上にも `+ Agent` を表示する。

### 6.2 Agent一覧

表示項目:

- Agent Type
- Instruction
- 状態
- 操作

Agent Type:

- Codex
- Claude
- Kiro

### 6.3 Agent追加

`+ Agent`押下時、右ペインを編集状態で開く。

入力項目:

- Agent Type
- Instruction

保存後、一覧に反映する。

### 6.4 Agent設定

画面上の設定項目は以下だけとする。

- Agent Type
- Instruction

以下は表示しない。

- Display Name
- Command
- Approval Bridge ON/OFF

理由:

- Display NameはAgent Typeと同一でよい
- CommandはAgent Typeごとに内部管理する
- Approval Bridgeは必須機能とする

### 6.5 内部Command

想定:

```text
Codex  -> codex
Claude -> claude
Kiro   -> kiro adapter または利用可能なCLI
```

MVPでは画面から変更させない。

環境差異対応のため、将来的には設定ファイルまたはAdvanced Settingsで上書き可能にする。

### 6.6 Instruction

Agentごとの基本方針。

例:

- 設計のみ行い、コードを変更しない
- 最小差分で実装する
- 必ずテストを実行する
- インフラ変更前にApprovalを要求する

Agent実行時の入力:

```text
Agent Instruction
+ Project Context
+ Milestone Context
+ Task内容
+ ユーザー追加指示
+ 実行ポリシー
```

### 6.7 Agent詳細

表示候補:

- Agent Type
- Instruction
- Status
- Capability
- Last Run
- Active Task
- Last Error

---

## 7. Projects

### 7.1 初期状態

Projectは0件。

中央に以下を表示する。

- 空状態アイコン
- `No projects imported`
- `+ Project`

ヘッダ右上にも `+ Project` を表示する。

### 7.2 Project追加

Projectは新規フォルダを生成するのではなく、既存ローカルフォルダを指定してインポートする。

入力項目:

- Local Folder
- Project Name

### 7.3 フォルダ選択

要件:

- OSのフォルダ選択UIを利用可能
- 選択した絶対パスを保存
- フォルダ末尾の名前をProject Nameへ自動入力
- Project Nameは後から変更可能
- 存在しないフォルダは登録不可
- 読み取りできないフォルダはエラー
- 同じパスの重複登録を防ぐ
- Git repositoryか検出する
- Gitでなくても登録可能

### 7.4 Project作成時の自動処理

1. Folder検証
2. Project作成
3. None Milestone自動作成
4. Git情報取得
5. Workspace情報保存
6. 初期スキャン
7. 設定ファイル検出

### 7.5 Project一覧

表示項目:

- Project Name
- Folder
- Milestones数
- Tasks数
- 操作

### 7.6 ホバー操作

Project行へフォーカスした際、以下を表示する。

- Milestonesアイコン
- Tasksアイコン
- Editアイコン

動作:

- Milestones: 対象Projectで絞り込んだMilestones画面へ
- Tasks: 対象Projectで絞り込んだTasks画面へ
- Edit: Project詳細ペインを編集状態で表示

### 7.7 Project詳細

表示項目:

- Name
- Folder
- Git root
- Git remote
- Milestones数
- Tasks数
- Last Run
- Default Milestone: None

---

## 8. Milestones

### 8.1 基本方針

Milestoneは任意。

Task作成時に未選択でもよい。

### 8.2 Milestones画面ヘッダ

右上:

- Projectプルダウン
- `+ Milestone`

### 8.3 一覧

表示項目:

- Milestone Name
- Progress
- Task Count
- Status
- 操作

### 8.4 None Milestone

- Project作成時に自動生成
- 削除不可
- 名前変更不可
- `default`バッジを表示可能
- Task未指定時の保存先
- 通常Milestoneへ後から変更可能

### 8.5 Milestone作成・編集

入力項目:

- Name
- Description
- Status
- Target Date 任意

### 8.6 詳細ペイン

表示:

- Project
- Name
- Description
- Status
- Progress
- Task Count
- Target Date
- Related Tasks

### 8.7 Taskへの遷移

Milestoneから対象Project・Milestoneで絞り込んだTasks画面へ遷移できる。

---

## 9. Tasks

### 9.1 表示形式

カンバン。

レーン:

- ToDo
- In Progress
- Review
- Done

### 9.2 ヘッダ

右上:

- Projectプルダウン
- Milestoneプルダウン
- `+ Task`

### 9.3 ProjectとMilestoneの連動

Project変更時:

1. Milestone候補を選択Projectのものへ更新
2. MilestoneをNoneへ戻す
3. Task一覧を再読込

Milestone選択肢:

- All
- None
- 通常Milestone

### 9.4 Task作成

入力項目:

- Title
- Description
- Project
- Milestone 任意
- Status
- Assigned Agent 任意
- Instruction 任意
- Acceptance Criteria 任意

Milestone未選択で保存可能。

未選択時はNone Milestoneへ紐づける。

### 9.5 Task詳細

表示項目:

- Title
- Project
- Milestone
- Status
- Assigned Agent
- Description
- Instruction
- Acceptance Criteria
- Latest Run
- Latest Trace
- Approval Status
- Agentとの会話履歴

### 9.6 Task編集

変更可能:

- Title
- Description
- Project
- Milestone
- Status
- Assigned Agent
- Instruction
- Acceptance Criteria

Project変更時:

- Milestone候補を変更後Projectへ切り替える
- 現在のMilestoneが存在しない場合はNoneへ変更

### 9.7 カンバン操作

基本遷移:

```text
ToDo -> In Progress -> Review -> Done
```

差し戻し:

```text
Review -> In Progress
Done -> In Progress
Any -> ToDo
```

Go実装の複雑さを抑えるため、MVPではドラッグ&ドロップを必須にしない。

代替:

- 詳細ペインでStatus変更
- カードメニューから変更
- キーボード操作

### 9.8 Agent実行

Task詳細から以下を実行可能。

- Agent選択
- 指示入力
- Run開始
- 追加メッセージ
- Stop
- Retry

Run保存項目:

- Project
- Milestone
- Task
- Agent
- Instruction
- Start / End
- Status
- Exit Code
- PID
- Working Directory
- stdout
- stderr
- Trace ID

---

## 10. Approvals

### 10.1 目的

Codex、Claude、KiroのPermission要求をtailagentへ集約する。

ユーザーは元ターミナルへ戻らずに回答できる。

### 10.2 一覧

表示項目:

- Agent
- Project
- Task
- Request Type
- Operation / Command
- Reason
- Risk
- Requested At
- Timeout
- Action

### 10.3 MVPアクション

- Allow once
- Deny

将来:

- Always allow for project
- Always allow for agent
- Allow during this task
- Deny rule

### 10.4 処理フロー

```text
Agent
 -> Permission Request
 -> Agent Adapter
 -> Approval Broker
 -> SQLiteにPending保存
 -> Approvals UI
 -> User Allow / Deny
 -> Agent Adapter
 -> 元AgentへDecision返却
 -> Traceへ保存
```

### 10.5 Timeout

既定動作:

- Deny
- RunへTimeout Error
- Trace記録
- Taskへ警告

### 10.6 Approval Bridge

必須機能。

Agent設定画面にはON/OFFを表示しない。

---

## 11. Traces

### 11.1 レイアウト

Datadog Logsを参考にする。

```text
左: Filter
上: 時系列折れ線グラフ
下: Trace / Log一覧
右: 選択したTrace詳細
```

### 11.2 ヘッダ右上

- Span
- Refresh

Span:

- Live
- 5 mins
- 30 mins
- 1 hour
- 6 hours
- 1 day
- 2 days
- 1 week

### 11.3 Filters

- Search
- Project
- Milestone
- Task
- Agent
- Status
- Event Type
- Time Range

Status:

- Running
- Success
- Error
- Waiting Approval
- Cancelled

Event Type:

- Agent Run
- Tool Call
- Approval Request
- Approval Decision
- stdout
- stderr
- File Change
- Test Result
- System Event

### 11.4 グラフ

MVP:

- 時間帯ごとのイベント件数
- 単一折れ線

将来:

- Agent別
- Status別
- Error Rate
- Approval待機時間
- Cost / Token

### 11.5 一覧

表示:

- Time
- Status
- Agent
- Project
- Task
- Event
- Message
- Duration

### 11.6 Trace詳細

表示:

- Trace ID
- Run ID
- Agent
- Project
- Milestone
- Task
- Status
- Start / End
- Duration
- Instruction
- Tool Calls
- stdout
- stderr
- Approval履歴
- Exit Code
- Changed Files
- Test Result
- Error

### 11.7 OpenTelemetry

Phoenixは必須にしない。

標準構成:

```text
Built-in Trace UI
SQLite
Optional OTel Exporter
Optional Phoenix / Jaeger / Tempo
```

内部TraceはOTelへ変換可能な構造にする。

---

## 12. Settings

MVPでは必須項目のみ。

- Workspace Root
- SQLite Database Path
- Approval Timeout
- Default Shell
- Max Concurrent Agents
- Trace Retention
- Optional OTel Endpoint

Agent CommandやApproval Bridgeは通常画面に出さない。

---

## 13. Agent Adapter

Agent差異を吸収する共通層を設ける。

```go
type AgentAdapter interface {
    Type() AgentType
    Validate(ctx context.Context) error
    Start(ctx context.Context, req RunRequest) (*RunHandle, error)
    Send(ctx context.Context, runID string, message string) error
    Stop(ctx context.Context, runID string) error
    Events(ctx context.Context, runID string) (<-chan AgentEvent, error)
    ResolveApproval(ctx context.Context, approvalID string, decision ApprovalDecision) error
}
```

Capability例:

- supports_streaming
- supports_approval
- supports_send_message
- supports_resume
- supports_tool_events
- supports_file_diff

UIはCapabilityに応じて操作を出し分ける。

---

## 14. ローカルプロセス管理

Go側で管理する。

- 子プロセス起動
- stdin
- stdout
- stderr
- Exit Code
- Signal
- Timeout
- Working Directory
- Environment Variables

最大同時実行数の初期値:

```text
2
```

RunのWorking DirectoryはProject Folder。

将来的にはTask単位Git Worktreeを追加。

---

## 15. データモデル

### agents

```sql
id
type
instruction
status
capabilities_json
created_at
updated_at
```

### projects

```sql
id
name
folder_path
git_root
git_remote
default_milestone_id
created_at
updated_at
```

### milestones

```sql
id
project_id
name
description
status
target_date
is_default_none
created_at
updated_at
```

### tasks

```sql
id
project_id
milestone_id
title
description
status
assigned_agent_id
instruction
acceptance_criteria
created_at
updated_at
```

DBでは`milestone_id`を必須にし、未指定時はNone Milestone IDを保存することを推奨。

### runs

```sql
id
project_id
milestone_id
task_id
agent_id
status
instruction
working_directory
process_id
exit_code
started_at
ended_at
trace_id
created_at
```

### approvals

```sql
id
run_id
agent_id
project_id
task_id
request_type
operation
reason
risk
status
decision
requested_at
decided_at
expires_at
```

### trace_events

```sql
id
trace_id
run_id
parent_event_id
event_type
status
message
attributes_json
started_at
ended_at
duration_ms
created_at
```

### run_logs

```sql
id
run_id
stream
sequence
content
created_at
```

---

## 16. 状態

Task:

```text
todo
in_progress
review
done
```

Run:

```text
queued
starting
running
waiting_approval
success
error
cancelled
timeout
```

Approval:

```text
pending
allowed
denied
expired
cancelled
```

Agent:

```text
not_configured
validating
ready
running
error
unavailable
```

---

## 17. エラー処理

### Agent未インストール

- Validateで検出
- Unavailable表示
- Run開始不可
- 設定方法を案内

### Project Folder不存在

- Project詳細に警告
- Run開始不可
- Folder再設定

### Process異常終了

- RunをErrorへ
- stderr保存
- Trace記録
- TaskへLast Error表示

### Approval返却失敗

- Approval Error
- 再送導線
- Run停止または待機
- Trace記録

### SQLite書込失敗

- UI通知
- ローカルファイルログへフォールバック可能

---

## 18. セキュリティ

- localhostのみへBind
- 外部公開ポートを既定で使わない
- SecretをTraceへ保存しない
- `.env`やTokenをマスキング
- Approval履歴を監査ログへ保存
- Denyを優先
- Timeout時はDeny
- Project Folder外アクセスを制限可能
- 危険コマンド検出を将来追加

---

## 19. 非機能要件

### パフォーマンス

- 一覧表示: 1秒以内
- 詳細ペイン表示: 300ms以内
- Approval反映: 1秒以内
- stdout表示遅延: 1秒以内

### データ保持

既定:

- Trace: 30日
- stdout / stderr: 30日
- Approval Audit: 90日以上
- 手動削除可能

### 対応OS

MVP:

- macOS

将来:

- Linux
- Windows

---

## 20. MVP範囲

含める:

- Agents
- Codex / Claude / Kiro
- Agent Instruction
- Projects
- ローカルFolder Import
- None Milestone
- Milestones
- Tasks
- Milestone任意
- Agent Run
- stdout / stderr
- Approvals
- Allow / Deny
- Traces
- SQLite
- 右オーバーレイペイン
- 空状態UI

含めない:

- クラウド同期
- 複数ユーザー
- RBAC
- GitHub同期
- PR自動作成
- 完全なWorktree管理
- 高度なPolicy Engine
- Cost / Token集計
- Phoenix必須連携
- モバイル対応
- Agent間完全自律会話

---

## 21. 実装優先順位

### Phase 1: UIと保存

1. Goプロジェクト
2. SQLite
3. データモデル
4. 共通レイアウト
5. 左サイドバー
6. ヘッダ
7. 右オーバーレイペイン
8. 空状態

### Phase 2: 管理機能

1. Agents
2. Projects
3. None Milestone
4. Milestones
5. Tasks
6. Settings

### Phase 3: Agent実行

1. Adapter Interface
2. Codex Adapter
3. Claude Adapter
4. Kiro Adapter
5. Process Manager
6. stdout / stderr

### Phase 4: Approval

1. Approval Broker
2. Queue
3. Allow / Deny
4. Timeout
5. Decision返却
6. Audit Trace

### Phase 5: Trace

1. Event保存
2. Filter
3. Graph
4. 一覧
5. 詳細
6. Retention

---

## 22. 受け入れ条件

### Agents

- 初期状態で中央に`+ Agent`
- Agentを追加すると一覧へ反映
- TypeとInstructionを編集可能
- 行クリックで詳細表示
- 同じ行の再クリックで閉じる
- 詳細ペイン表示時も一覧幅が変わらない

### Projects

- 初期状態で中央に`+ Project`
- Folderを指定可能
- Folder名をProject Nameへ自動設定
- 保存後に一覧へ反映
- None Milestoneを自動作成
- Project行からMilestones / Tasksへ遷移可能

### Milestones

- Projectで絞り込み可能
- Noneが存在
- Noneは削除不可
- 通常Milestoneを追加・編集可能

### Tasks

- Projectを指定して作成可能
- Milestone未選択で作成可能
- 未選択時はNoneへ紐づく
- 後からMilestone変更可能
- Project変更時にMilestone候補が連動
- Agentを割り当てて実行可能

### Approvals

- Approval要求を表示
- Allow / Deny可能
- 結果をAgentへ返却
- Traceへ保存
- Timeout時にDeny

### Traces

- Runを一覧表示
- Project / Agent / Statusで絞り込み
- Span変更
- Refresh
- stdout / stderr表示
- Approval履歴表示

---

## 23. 推奨技術構成

### Core

- Go
- SQLite
- `os/exec`
- Context
- Goroutine
- Channel

### UI候補

最有力:

- Wails
- HTML / CSS / JavaScript
- Go Backend

代替:

- Fyne
- Gio
- Bubble Tea

現在のHTMLモックに近いUIを保つならWailsが適している。

### Trace

- SQLite
- 独自Trace Model
- OTel Exporterは任意

---

## 24. 推奨ディレクトリ構成

```text
tailagent/
├── cmd/
│   └── tailagent/
├── internal/
│   ├── agent/
│   │   ├── adapter.go
│   │   ├── codex/
│   │   ├── claude/
│   │   └── kiro/
│   ├── approval/
│   ├── project/
│   ├── milestone/
│   ├── task/
│   ├── run/
│   ├── trace/
│   ├── process/
│   ├── storage/
│   │   └── sqlite/
│   ├── policy/
│   └── ui/
├── migrations/
├── assets/
├── configs/
├── tests/
├── go.mod
└── README.md
```

---

## 25. 最終定義

tailagentは、Codex・Claude・KiroなどのAIコーディングエージェントをローカルで管理し、Project・Milestone・Task単位で作業を割り当て、Permission、ログ、Traceを一元管理するツールである。

Milestoneは任意とし、未指定TaskはProjectごとのNone Milestoneへ自動的に紐づける。

Approval Bridgeは必須機能とし、AgentからのPermission要求へtailagent上で回答できるようにする。

UIはGoでの実装を考慮し、一覧、カンバン、右オーバーレイペイン、フィルタ、折れ線グラフを中心とする。
