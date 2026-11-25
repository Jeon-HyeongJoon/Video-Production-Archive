# 11. 로컬 CLI Archivist Host 설계

원본 분류 시험의 진행량·실패 사유·행사별 게시 증적은 Windows Host에서 읽기 전용으로 조회한다. 운영 ArchiveRun의 채널별 MCP 계약과 구체적인 시험 명령은 [원본 분류 정책](17-source-classification-policy.md#4-archivist가-인수할-근거)에 있다.

## 1. 결정

v1의 Archivist 실행 경로는 Orca 전체 애플리케이션을 운영 의존성으로 두지 않는다. Archive Control와 별도인 `archivist-host`가 필요한 최소 런타임 기능만 제공하고, Orca CLI는 같은 Port를 구현하는 선택형 backend로 둔다.

이 결정의 목적은 로컬 Codex·Claude 같은 CLI의 도구 사용과 세션 재개 능력은 유지하면서 영상 기록 시스템에 필요하지 않은 개발 기능의 결합을 피하는 것이다.

```text
Archive Workspace
   ├── button/form/API ──────────────────> Command Bus
   └── chat ─> ArchivistSession / ArchivistTurn ─> Archivist Gateway
                                               │
Archive Control ── Application Services ── Command Bus
   ▲      ▲                              ▲
   │      └── authenticated MCP ─────────┘
   │
   └── outbound authenticated stream ── Archivist Host
                                           │
                                           ├── Embedded Core → archive-archivist ChatGPT/local loop
                                           ├── Embedded Core → Codex / Claude CLI
                                           └── Orca CLI backend (선택)
```

Archivist Host는 Archivist 역할이 아니다. 특정 PC에서 CLI 프로세스와 세션을 감독하는 서비스 구성요소이며, 미디어 Step을 실행하는 Worker와도 분리한다.

## 2. 상태 소유권

| 소유자 | 영속 상태 | 소유하지 않는 것 |
|---|---|---|
| Archive Control | ArchivistSession, ArchivistTurn, ArchivistTurnLease와 fencing token, ArchivistToolCall, ActionCard, Approval, Command 상관관계 | PTY handle, CLI 화면 상태 |
| Archivist Host | PC 등록 identity, 허용 CLI·작업 root, 암호화된 CLI resume token, 마지막 확인 event cursor | Job·Step 상태, 승인 정책, Command 결과의 원장 |
| Runtime backend | 한 CLI process/PTY, backend의 임시 runtime handle, 구조화된 입출력 변환 | durable session ID, 업무 멱등성 |
| MCP Adapter | 허용된 도구 schema와 Capability 변환 | 범용 shell, 임의 파일 경로 접근, 승인 우회 |

Control의 `archivist_session_id`와 `archivist_turn_id`만 영속 identity다. Orca terminal ID, PID, PTY handle 같은 `runtime_handle`은 프로세스 수명에 한정하며 DB identity로 저장하지 않는다.

Archivist 구성 화면은 이름·`archivist_host_id`·감지된 `cli_id`만 받아 Agent를 추가하고, 활성 Agent별 제거 조작을 둔다. 제거는 끝나지 않은 Turn과 결정 대기 ActionCard가 없을 때만 ArchivistSession을 활성 목록에서 제외하는 retirement이며 Session·Turn·Tool Call 이력을 삭제하지 않는다. 제거된 Session은 새 Turn과 SYSTEM 재호출을 받지 않지만 이미 시작된 ArchiveRun은 사람 조작 경로에서 계속된다. `runtime_backend`와 `workspace_root_id`는 사전 등록된 Host 설정에서 결정하고 화면이 임의 값을 받지 않는다. 고정된 Host가 offline이면 다른 PC로 자동 우회하지 않고, 같은 PC의 로컬 설정에 Windows 예약 작업이 고정돼 있을 때만 담당자의 Agent 추가·선택·다시 켜기에서 그 작업의 시작을 요청한다. 이 시작은 Session 재배정이 아니며 Host가 online이면 중복 process를 만들지 않는다. `CTL-006`, `CTL-008~009`, `DSH-012`, `DSH-042`

같은 이름·`archivist_host_id`·`cli_id`의 활성 Archivist 추가 요청은 기술 Session ID가 달라도 최초 ArchivistSession을 재사용한다. 화면은 추가 요청이 끝날 때까지 제출을 잠가 빠른 이중 클릭을 막고, Control은 재전송을 최종 방어한다. 제거된 Session은 같은 구성의 새 추가를 막지 않는다. `CTL-006`, `DSH-012`

## 3. 최소 런타임 코어

v1 기본 backend인 Embedded Core가 제공할 기능은 다음뿐이다.

| 필수 기능 | 계약 |
|---|---|
| process/PTY 수명주기 | 허용 목록의 실행 파일과 인수 배열만 시작·종료 |
| 세션 연결 | durable ArchivistSession을 하나의 로컬 CLI 세션과 연결 |
| turn 전달 | request ID로 중복 없이 구조화된 입력 전송 |
| 단계 이벤트 | `INPUT_ACCEPTED`, `TURN_STARTED`, `TURN_PROGRESS`, `TURN_WAITING`, `TURN_COMPLETED`, `TURN_FAILED` 보고 |
| cursor replay | 단조 증가 cursor 이후의 정제된 이벤트를 재전송 |
| CLI별 재개 | native resume token 우선, 없으면 정제된 checkpoint 사용 |
| 재시작 조정 | stale runtime handle을 폐기하고 durable session으로 다시 해석 |

`archive-archivist`는 Embedded Core가 실행하는 허용 CLI 중 하나다. 이 바이너리는 `chatgpt` Provider에서는 OAuth Profile의 access token으로 Responses API를 사용하고, `local` Provider에서는 같은 Host의 인증 없는 loopback OpenAI 호환 Chat Completions endpoint를 사용한다. 두 경로 모두 Host loopback MCP proxy 사이의 제한된 도구 루프를 수행하고 별도 Session 저장소를 만들지 않으며 한 process가 ArchivistTurn 하나만 처리한다. `EMBEDDED_CORE`는 process 감독 방식이고 model Provider 선택은 CLI 고정 인수이므로 RuntimeBackend enum을 추가하지 않는다. `AGT-016~021`, `LRH-016`

Host 설정은 같은 `archive-archivist` executable을 `archive-gpt`와 `archive-local` 같은 서로 다른 CLI ID로 동시에 등록할 수 있다. 각 ArchivistSession은 사람이 고른 CLI ID에 고정되고 Turn 중 자동 전환하지 않는다. local 경로는 `--provider local --api-base http://127.0.0.1:<port>/v1 --model <slug>`를 고정 인수로 사용하며 model endpoint의 function tool call 지원이 필수다.

Windows의 `archive-codex` CLI Adapter는 일회성 `codex exec` 화면을 긁지 않고 Codex `app-server --listen stdio://`의 구조화 protocol을 사용한다. ArchivistSession 하나를 Codex의 영속 native thread 하나에 대응시키고, 실행 process가 끝나도 service 계정 전용 상태 디렉터리에 Session→thread reference만 원자적으로 보존한다. 다음 ArchivistTurn은 새 app-server process에서 `thread/resume`으로 같은 thread를 열고 `turn/start`로 입력한다. Control의 ArchivistSession·Turn이 업무 identity이고 native thread reference는 CLI 대화 재개용일 뿐 Command·Tool Call 멱등성의 원장이 아니다. prompt, raw terminal, reasoning, credential과 실제 파일 경로는 이 mapping에 저장하지 않는다. `LRH-018`

Windows Archivist Host는 로그인 사용자의 대화형 예약 작업으로 상주하되 Codex app-server process는 Turn이 있을 때만 시작한다. Host 예약 작업은 로그인 trigger와 1분 복구 trigger를 함께 가져 비정상 종료 뒤 터미널 조작 없이 다시 시작한다. `archive-codex`는 Turn 시작 전에 같은 사용자 문맥의 `codex login status`만 검사하고, 로그인 정보가 없으면 Host를 종료하지 않은 채 그 Turn에 `CODEX_LOGIN_REQUIRED`와 정제된 안내만 남긴다. `LRH-020`, `LRH-022`

app-server에는 사용자 설정의 MCP 목록을 비우고 Archive stdio MCP 하나만 필수로 주입한다. local terminal·파일 변경·설치된 Skill·보조 Agent는 켜고 `never` approval과 `danger-full-access`를 thread·Turn마다 고정한다. browser·computer use·apps·plugins·외부 connector·image 계열은 끈다. Codex는 Host의 고정 workspace와 같은 로그인 계정 권한 안에서 native 도구를 사용하며 Archive 업무 조회·선택·판정·실행·승인·영속 기록은 Archive MCP를 통해서만 수행하도록 developer instruction을 고정한다. Archive MCP bridge는 Control이 광고하고 로컬 schema registry가 허용한 업무 도구만 반환하며, 호출을 현재 Session·Turn·fencing과 결정적 Tool Call identity가 있는 Host loopback request로 바꾼다. Control service credential은 Codex process에 전달하지 않는다. `LRH-019`, `MCP-006`

다음 Orca 기능은 v1 Archivist Host에 포함하지 않는다.

- Git worktree·branch·repository 생애주기
- 여러 terminal·pane 관리
- 내장 browser와 desktop UI
- artifact 게시·공유
- automation scheduling과 세션 검색 색인
- 다중 개발 Archivist orchestration·handoff
- Electron 또는 Orca 애플리케이션 프로세스 의존성

Orca 코드를 재사용할 때는 PTY, cursor buffer, request delivery, resume record처럼 독립시킬 수 있는 모듈만 추출한다. Orca 내부 UI 모델이나 worktree identity를 제품 계약으로 노출하지 않는다. 직접 추출할 수 없거나 결합 비용이 크면 같은 Port의 작은 구현을 작성한다.

## 4. Runtime backend Port

구체 backend는 다음 의미의 Port를 구현한다. 메서드 이름은 구현 언어에 맞출 수 있지만 관찰 가능한 결과는 바꾸지 않는다.

```text
Open(SessionSpec) → RuntimeSessionRef
Submit(RuntimeSessionRef, TurnRequest) → DeliveryReceipt
ReadEvents(RuntimeSessionRef, after_cursor) → RuntimeEvent[]
Resume(RuntimeSessionRef, ResumeCheckpoint) → RuntimeSessionRef
Close(RuntimeSessionRef) → CloseReceipt
```

`SessionSpec`에는 durable session ID, CLI ID, workspace root ID, MCP endpoint reference만 들어간다. credential 원문과 unrestricted path는 들어가지 않는다. `TurnRequest`에는 durable turn ID, request ID, prompt와 최소 context가 들어간다.

Orca CLI backend를 사용할 때는 다음 제약을 둔다.

- ArchivistSession 하나에 terminal 하나만 사용한다.
- terminal handle은 runtime-scoped 값으로 취급하고 restart 또는 stale 응답 뒤 `terminal list/show`로 다시 찾는다.
- prompt 전송의 `input_accepted`는 전달 수락일 뿐 실행 시작이 아니다. 별도 `turn_started`가 있어야 ArchivistTurnState.RUNNING으로 바꾼다.
- 제출 관찰은 최초 request ID로 계속하고 timeout 때문에 prompt를 다시 보내지 않는다.
- `tui-idle`이나 화면 문자열만으로 업무 완료를 판정하지 않는다. backend가 명시적 완료·실패 이벤트를 제공하지 않으면 Orca core에 그 이벤트를 추가하거나 MCP 완료 protocol을 사용한다.
- `terminal read`의 cursor는 출력 재연결에만 쓰며 Control의 Event ID를 대체하지 않는다.

## 5. ArchivistTurn 전달과 fencing

```text
아카이브 작업 공간이 ArchivistTurn 제출
→ Control이 ArchivistTurnState.QUEUED를 먼저 확정
→ 연결된 Host가 ArchivistTurnLease claim
→ Control이 fencing token 발급
→ Host가 같은 request ID를 backend에 한 번 전달
→ INPUT_ACCEPTED
→ TURN_STARTED
→ TURN_PROGRESS / tool call / waiting
→ TURN_COMPLETED 또는 TURN_FAILED
```

동시에 유효한 ArchivistTurnLease는 하나뿐이다. lease 만료 뒤 새 claim에는 더 큰 fencing token을 발급하고, 이전 Host나 이전 process가 보낸 event·Tool Call·완료 결과는 거부한다.

ArchivistTurnState는 다음 전이만 허용한다.

```text
QUEUED → LEASED → SUBMITTED → RUNNING
RUNNING → WAITING → RUNNING
QUEUED | LEASED | SUBMITTED | RUNNING | WAITING → CANCELLED
RUNNING | WAITING → COMPLETED | FAILED
```

`INPUT_ACCEPTED`는 SUBMITTED, `TURN_STARTED`는 RUNNING에 대응한다. Host heartbeat나 PTY idle은 ArchivistTurn 상태 이벤트가 아니다.

## 6. 재시작과 세션 재개

Host는 native CLI resume token을 OS Secret Store 또는 해당 서비스 계정만 읽을 수 있는 암호화 저장소에 둔다. Control에는 token reference와 정제된 checkpoint만 저장한다.

Host 또는 backend 재시작 시 다음 순서를 따른다.

1. runtime handle을 신뢰하지 않고 ArchivistSession·backend·CLI ID로 로컬 세션을 다시 해석한다.
2. native resume token이 있으면 해당 CLI의 공식 resume 기능을 사용한다.
3. token이 없으면 사용자 목표, 확정된 업무 상태, 완료된 ArchivistToolCall 결과 요약과 미완료 호출 ID만 포함한 checkpoint로 새 CLI 세션을 연다.
4. 완료된 ArchivistToolCall은 결과를 replay하고 다시 실행하지 않는다.
5. 진행 중 호출만 같은 idempotency key와 현재 fencing token으로 조정한다.

Windows에서 Host process 자체가 없으면 아카이브 작업 공간의 Agent 선택 또는 추가가 Control의 고정 Host 시작 API를 호출한다. Control은 boot config의 `windows_task_name`만 `schtasks.exe /Run /TN` 인수 배열로 실행하며 브라우저가 task 이름이나 실행 파일을 보내지 못하게 한다. 예약 작업의 반복 trigger도 같은 Host를 복구하므로 화면 시작 요청이 유실되어도 다음 주기에 회복한다. `CTL-009`, `DSH-042`

대화 원문 전체, credential, 불필요한 파일 경로와 raw terminal screen은 checkpoint에 넣지 않는다.

이미 전달된 Turn의 lease를 만료 뒤 다시 claim하면 Control은 `delivered_request_id`, 마지막 runtime event cursor와 새 fencing token을 반환한다. Host는 같은 사용자 입력을 새 입력으로 확정하지 않으며 `INPUT_ACCEPTED`를 반복하지 않는다. native reference가 없는 Embedded Core는 원래 목표와 정제된 checkpoint로 backend를 다시 열고, `TURN_STARTED`부터 마지막 cursor 다음 값으로 보고한다. Control은 이미 전달된 Turn에 한해서 새 lease의 `TURN_STARTED`를 `LEASED → RUNNING` 재개 전이로 허용한다.

ChatGPT plan의 직접 HTTP 경로는 `previous_response_id`를 사용하지 않는다. `archive-archivist`는 현재 Turn의 prompt, Host가 준 최소 checkpoint, 한 process 안에서 완료된 model output item과 Tool Call receipt만 다음 Responses round의 `input`으로 다시 보낸다. process가 끊기면 기존 Control receipt를 기준으로 checkpoint 실행을 새로 시작하며 같은 결정적 Tool Call ID와 멱등 key로 완료 효과를 replay한다. 긴 ArchiveRun은 model polling으로 유지하지 않고 기존 SYSTEM 재호출 Turn으로 이어 간다.

local 경로도 현재 Turn 밖의 model conversation을 소유하지 않는다. 같은 process 안에서 assistant tool call과 MCP receipt를 Chat Completions message로 다음 round에 전달하고, process가 끊기면 같은 Control receipt 기반 checkpoint에서 다시 시작한다. 인증 없는 endpoint는 loopback HTTP만 허용하고 redirect를 따르지 않으므로 model 입력이 Host 밖으로 이동하지 않는다. `AGT-021`, `LRH-016`

Codex app-server 경로는 ArchivistSession에 대응한 native thread를 process 경계 뒤에도 재개한다. 같은 Control Turn이 중단되어 checkpoint로 다시 claim되면 같은 native thread에 checkpoint 입력을 추가하고, 이미 완료된 Archive Tool Call은 Control receipt와 결정적 call identity로 replay하여 효과를 중복하지 않는다. app-server가 반환한 reasoning delta, command text·output, raw stderr와 filesystem path는 버린다. 공개 `agentMessage` commentary, Archive MCP Tool item과 native `commandExecution`·`fileChange`·`collabAgentToolCall`은 각각 업무 도구 이름·`터미널 명령 실행`·`파일 변경`·`보조 Agent 작업`으로만 정규화한다. terminal `agentMessage`만 `final_response`가 될 수 있다. `LRH-018~019`

`TURN_COMPLETED`는 상태 문자열만 보내지 않는다. backend는 사람이 읽을 수 있는 `summary`와 정제된 `result_ids`를 가진 구조화 `final_response`를 반환하고, Host는 현재 fencing token과 단조 cursor로 이를 Control에 보낸다. Control은 final response가 없는 완료를 거부하고 ArchivistTurn과 같은 transaction으로 저장·투영한다. raw stdout, credential, resume token과 실제 filesystem path는 final response에 넣지 않는다.

`TURN_PROGRESS`는 raw terminal cursor를 Control에 복제하는 통로가 아니다. Runtime Adapter가 사용자에게 공개할 내용으로 명시적으로 만든 `summary`만 `ProgressUpdate`로 보고한다. native 도구 진행은 도구 종류만 남기고 명령·diff·하위 Agent prompt를 복제하지 않는다. `TURN_FAILED`는 raw error 문자열 대신 정규화 `code`와 정제 `summary`를 가진 `FailureReport`를 요구한다. Control은 PROGRESS·FAILURE를 `ArchivistTurnEntry` append-only 기록으로 Turn cursor·상태와 한 SQLite transaction에 확정하고 중복 cursor를 늘리지 않는다. Entry schema에는 stdout·stderr·credential·filesystem path 필드가 없다. 계약 fixture와 Codex app-server fixture는 이 구조화 event stream을 검증한다. 실제 계정으로 인증된 Codex·Claude 정제 증적은 `LRH-014` acceptance 전까지 planned다. `LRH-013~014`, `LRH-018~019`

Archivist가 작업 중단 원인을 요청받거나 자동 복구 Turn을 처리하면 `archive.work.diagnostics.get.v1`을 먼저 사용한다. 응답은 증적 ID가 있는 확인된 `facts`, 예측·확인 방법과 증적 ID basis가 있는 `hypotheses`, 설명용 `suggested_actions`, 실행 가능한 `recovery_actions`를 분리한다. Archivist는 확정 진술에 해당 증적 ID를 근거로 사용하고 가설을 사실로 바꾸지 않는다. `recovery_actions`가 있으면 필드를 바꾸지 않고 `archive.work.recovery.apply.v1`에 제출한 뒤 진단을 다시 읽어 복구 증적 ID·복구 Job ID와 결과 상태를 보고한다. 목록에 없는 retry·cancel·분류·단계 시작을 복구로 대신하지 않는다. subprocess의 정지를 직접 관측하지 못한 경우 확정 원인 대신 현재 확인 가능한 실행 리스·Worker 상태와 추가 확인 방법을 보고한다. 새 드라이브 자동 제안은 현재 후보 조회→원본 intake session→적격 아카이브와 명시적 폴더 범위 고정→아카이브 시작 ActionCard 제안 순서로 끝내며, 카드가 실행되기 전에는 시작했다고 말하지 않는다.

진단에 `AVAILABLE` process 실패 증적이 있으면 Archivist는 원인을 추측하기 전에 해당 증적 ID로 `archive.work.failure_evidence.get.v1`을 호출한다. 전사는 반환된 구조화 원인과 실제 시도·종료 코드를 인용하고, 그 밖의 process는 정제 tail에서 확인한 내용을 인용한다. 상태 조회 요청에는 진단과 가능한 조치를 보고한다. 복구 요청 또는 REMEDIATION Turn에서 현재 진단의 action·basis로 복구를 실행하고 연결 Job의 최종 단계를 확인한다. 상세 상태가, `CAPTURE_FAILED`·`EXPIRED`·`LEGACY_UNRESOLVABLE`이면 상세가 기록되지 않았거나 더는 가용하지 않다고 보고한다. “다”, “모두”, “전체”처럼 범위가 여러 작업인 질문은 현재 Turn에 WorkReference가 있어도 먼저 `archive.system.status.get.v1`으로 전체 미해결 범위를 확인한 뒤 각 WorkReference의 진단과 실패 상세로 좁힌다. `MCP-012~013`

## 7. 도구 호출 경계

CLI는 Host가 loopback에 연 MCP proxy에만 연결한다. proxy는 Host service credential을 사용해 Control의 인증된 MCP endpoint로 outbound 연결하고 다음 상관관계를 추가한다.

```text
archivist_host_id
archivist_session_id
archivist_turn_id
archivist_tool_call_id
archivist_turn_lease_fencing_token
idempotency_key
```

Control의 MCP Adapter는 도구를 좁은 Application Service 입력으로 변환한다. 조회는 정제된 결과를 반환하고 상태 변경은 사람의 직접 명령과 같은 Command Bus를 통과한다. `archive.work.reference.get.v1`은 현재 Turn에 고정된 `WorkReference`만 반환해 작업 도중 이관 대상의 종류·ID·revision을 채팅 본문과 분리한다. 선택 범위 또는 `WorkReference`가 없는 유효한 Turn을 조회하면 `bound=false`를 반환하고 Tool Call을 `COMPLETED`로 기록한다. `archive.system.status.get.v1`은 선택 없이 Worker·Archivist Host 연결과 자원, 작업 상태별 수, 미해결·정체 의심 작업의 정제 진단과 증적 ID를 반환하며 어떤 참조나 업무 상태도 만들지 않는다. 잘못된 Turn 문맥과 Adapter 오류만 조회 실패이며, 선택을 요구하는 실행 Capability는 계속 ActionCard 또는 충돌로 차단한다. 모든 영속 업무를 서버 Adapter 없이 직접 실행하는 공통 Capability Registry는 `APP-001` planned 경계다. 제공 도구는 업무 의미 allowlist로 제한하며 `run_shell`, 임의 terminal 입력, 임의 path 읽기·이동·삭제를 제공하지 않는다. `COL-004`, `MCP-008`, `MCP-012`

ArchivistToolCall receipt가 이미 있으면 최초 결과를 반환한다. timeout이나 CLI 재개 뒤에도 같은 호출이 Command를 다시 만들지 않는다. 인증된 MCP Capability는 allowlist의 비파괴 `PROCESS` Command를 직접 실행할 수 있다. 파괴적 작업과 `GATED` 작업은 ActionCard를 만들 수만 있고, 별도의 사람 Approval과 최신 expected revision 없이 실행하지 않는다.

드라이브 선택 없이 ArchiveRun 작업 참조만 고정한 Turn에서도 진단은 작업 상세와 같은 `work_state`와 `result_count`를 반환한다. `ARCHIVE_RUN_OUTCOME` 증적은 실행과 결과를 연결한다. 입력 순번·출력 위치와 실패 상세를 함께 인용할 때는 Job·Step·fencing token을 맞춘다. `MCP-018`

재개 리스의 유효성은 매 호출 확인한다. 새 토큰에서 같은 Session·Turn·Tool Call의 최초 명령과 업무 입력이 일치하면 원래 영수증을 반환한다. 재제안에는 저장된 카드의 현재 결정이 포함된다. 명시한 사람이 실행한 보관 시작은 Command 행위자에 그 사람을, 선택 출처에는 Turn에 고정한 선택 행위자를 기록한다. Windows 재시작 인수는 ADVISORY 제안과 실행 전후의 실제 영수증을 조회한다. `MCP-019`, `LRH-011`

재개된 CLI가 같은 Tool Call ID와 ActionCard ID를 다시 보고하면 저장소는 새 행을 만들지 않는다. 기존 ActionCard 제안 내용이 같으면 현재 투영 상태를 보존하고, 특히 `COMPLETED`를 `PENDING`으로 되돌리지 않는다. 같은 ID의 제안 내용이 다르면 idempotency 충돌로 거부한다.

## 8. 아카이브 작업 공간 계약

아카이브 작업 공간은 Runtime 상태를 보기만 하는 화면이 아니다. 사람은 ArchivistSession 없이 버튼·폼·API로 Job과 도메인 Command를 직접 시작할 수 있고, 채팅으로 ArchivistTurn을 만들어 로컬 CLI Archivist에게 같은 업무의 조회·계획·실행을 지시할 수 있다. 두 경로는 같은 Command Bus, Event 원장과 작업 상세를 사용한다.

Archive Workspace 영역은 다음 영속 상태를 투영한다.

```text
Host: ONLINE | BUSY | OFFLINE | DISABLED
Session: host, backend, cli, workspace, last_turn
Turn: QUEUED | LEASED | SUBMITTED | RUNNING | WAITING | COMPLETED | FAILED | CANCELLED
Tool Call: 이름, 대상, 상태, 정제된 결과
Action Card: 제안 Command, 대상, expected revision, 사유, 근거, 승인 필요 여부
```

Host의 claim loop와 heartbeat loop는 CLI 실행 대기 때문에 서로 막히지 않는다. Turn을 claim한 뒤 CLI process가 수 분 동안 실행되더라도 Host는 설정된 heartbeat 주기로 Control에 계속 연결 상태를 갱신한다. Turn lease 갱신은 실행 fencing을, Host heartbeat는 연결 표시와 다음 claim 가능성을 각각 소유하며 하나를 다른 것으로 대신하지 않는다. `LRH-021`

Turn, Tool Call, ArchiveRun은 서로 다른 생애주기다. 개별 Tool Call 실패만으로 Turn이나 ArchiveRun 실패를 추론하지 않고, Agent 응답의 완료는 ArchivistTurn의 terminal event, 아카이브 처리의 완료·실패는 ArchiveRun과 하위 Job의 영속 상태로 표시한다. 선택적 문맥의 미고정 조회는 실패 Tool Call 자체를 만들지 않는다.

채팅 입력은 ArchivistTurn을 만들고 Control에 확정된 뒤 Host로 전달한다. 채팅 텍스트의 “진행”은 Archivist에게 후속 Tool Call을 요청할 수 있지만 Approval은 아니다. 승인 필요한 명령은 ActionCard의 명시적 조작만 Approval 또는 Command 후보를 만들며 제출 시 현재 리비전을 다시 검사한다.

Archive Intake Turn은 담당자가 화면에서 고른 참조 또는 인증된 Archivist가 현재 후보 MCP로 제출한 Worker·원본·아카이브 Storage·폴더 범위를 구조화된 ID와 revision으로 고정한다. 이미 진행 중인 Job 또는 ArchiveRun 상세에서 지시하면 별도의 `WorkReference`가 같은 Turn에 고정된다. Archivist는 본문을 실행 대상 필드로 직접 쓰지 않고 후보 조회 결과에서 선택을 먼저 고정한 뒤, 그 참조 안에서 드라이브·폴더 조회, 작업 상태 조회, 1·2단계 시작, 현재 HOLD 목록·단일 구조화 근거 조회와 명시적 분류 Capability만 호출한다. 오래되거나 후보 밖인 선택과 선택 밖 분류는 거부한다. 노이즈 후보는 고정 범위 안에서도 Archivist가 대응·미확인 근거와 가능한 선택지를 제시하고 담당자의 화면 선택을 기다린다. `COL-003~004`, `MCP-003~005`

고정 선택으로 시작하는 ArchiveRun은 인증된 Archivist의 `mcp` Command 경로로만 Control Runtime에 들어가며 사람의 직접 `APPLICATION` 경로와 교차 사용할 수 없다. `MCP-007`

긴 작업은 한 Turn 안에서 기다리지 않는다. Archivist가 1단계를 시작한 Turn을 완료한 뒤 Control은 같은 Session에 1단계 완료·전체 완료·새 HOLD 계기별 SYSTEM 재호출 Turn을 정확히 한 번 제출한다. ArchiveRun을 연결할 때 최초 Turn에 `work_id=archive_run_id`, `origin_turn_id`, `ARCHIVE_RUN_STARTED` 계기를 저장하고 각 SYSTEM Turn은 같은 ID와 명시적 재호출 계기를 저장한다. 화면은 prompt 문장을 해석하지 않고 이 `ArchiveRunWorkContext`로만 Turn을 묶는다. Host가 offline이면 Turn은 QUEUED로 남고 Worker의 아카이브 실행은 계속된다. 재호출 전달은 기존 ArchivistTurnLease·fencing·cursor 규칙을 그대로 사용한다. `LRH-012`

Control은 완료 계기 재호출과 별도로 자동 제안 조정기를 실행한다. Worker의 완전한 storage inventory가 Control에 확정되면 내부 신호가 조정기를 즉시 깨우며, Control 재시작·신호 유실과 시간 기반 작업 진단을 위해 낮은 빈도의 reconciliation도 수행한다. 프론트는 이 계기를 만들지 않고 결과만 투영한다. 외부 webhook은 이후 같은 확정 신호의 추가 sink로 연결할 수 있지만 ArchivistTurn 중복 방지와 업무 명령 권한을 대신하지 않는다. 조정기는 현재 projection만 읽고 다음 두 계기를 평가한다.

- ArchivistSession의 최신 `WorkReference`가 가리키는 진행 중 작업에서 일정 시간 새 progress 증적이 없거나 Worker·ExecutionLease·ResourceReservation 결합 진단이 이상을 보고한 경우
- `CONNECTED`·`READY` 원본 드라이브에 영상 폴더와 접근 가능한 ARCHIVE_PRIMARY 후보가 생긴 경우

자동 제안은 같은 work revision·진단 code 또는 workspace drive·candidate revision·folder discovery revision fingerprint마다 SYSTEM Turn 하나만 만든다. 작업 계기는 그 작업을 이미 참조한 Session에, 새 드라이브 계기는 제거되지 않은 Session 가운데 마지막 Turn 갱신 시각이 가장 최근인 하나에 보낸다. 활성 Session이 없으면 영속 작업이나 드라이브 상태를 바꾸지 않고 다음 평가를 기다린다. 작업 진단으로 Turn을 만들 때 당시의 Job·단계·실패 차수와 상태, 사실·원인·가설 및 가능한 조치의 코드를 함께 저장한다. 대화는 이 기록을 비교하며 보고 문구나 관측 시각만으로 변화 여부를 추측하지 않는다. Host가 offline이어도 Turn은 QUEUED로 남는다. `LRH-023~024`

SYSTEM Turn은 계기별 도구 정책을 고정한다. 새 드라이브 제안은 `ADVISORY`로 작업 진단·작업 상태·현재 후보 조회와 read-only intake session·현재 ID/revision 선택 준비만 허용하고, `archive.run.start.v1`은 PENDING ActionCard로 바꾼다. 작업 이상은 `REMEDIATION`으로 고정 작업 조회와 현재 진단의 `archive.work.recovery.apply.v1`만 허용한다. 복구 도구는 fingerprint·Job revision·선택적 reservation·fencing token·basis가 모두 같은 현재 action만 적용한다. 만료 실행은 같은 Step을 조정하고, 일시적 패키지·카탈로그 종결 실패는 원래 Job을 보존한 제한 복구 Job으로만 이어간다. 취소·HOLD 분류·STT 시작·입력 실패 retry·일반 Job 생성을 거부한다. 사람이 실패 기록에서 요청한 진단은 Job ID·revision을 고정하고 DIAGNOSTIC으로 저장한다. 작업 참조·상태·실패 근거는 조회할 수 있고 복구와 분류 등 변경 호출은 거부 기록을 남긴다. `LRH-025`, `MCP-016` 일반 사용자 Turn과 기존 단계 완료 SYSTEM 재호출 Turn은 `STANDARD` 정책을 유지하고 정책이 없는 기존 영속 Turn도 호환상 `STANDARD`로 읽는다. `MCP-009~011`, `WFL-015`

Agent 대화 칸은 Orca native chat과 같은 웹 셸형 채팅으로 취급한다. raw ANSI 화면을 iframe처럼 복제하는 셸이 아니라, 로컬 CLI의 구조화된 Turn·공개 assistant message·Tool Call·결과를 웹에서 조작하고 읽는 투영이다. 선택한 Session 이름에는 실제 CLI ID를 함께 표시해 Host 연결과 model CLI 선택을 구분한다. 입력은 Enter로 전송하고 Shift+Enter만 줄바꿈하며 IME 조합 확정 Enter는 전송하지 않는다. 업무는 사람의 지시와 최신 응답만 펼쳐 두고, 실행 중이면 진행 중 Turn의 마지막 진행 한 줄을 함께 보인다. 정제된 진행 항목·SYSTEM 재호출·Tool Call·이전 응답·처리된 ActionCard는 Tool Call 수·재호출 수가 있는 접힌 영역에 보존한다. 업무 문맥의 `ARCHIVE_RUN_COMPLETE` Turn이 종료되면 Orca의 settled-turn 규칙처럼 최초 사용자 지시와 마지막 최종 응답 또는 최종 구조화 실패만 남기고 접힌 영역에 소요 시간을 더한다. 선택 참조는 참조 수가 있는 접힌 줄에 둔다. `PENDING` ActionCard는 업무가 종료됐더라도 접힘 밖에 남겨 사람이 계속 조작할 수 있게 한다. `DSH-011`

사람이 직접 만든 Command Event도 같은 Job 문맥과 다음 ArchivistTurn의 정제된 context에 포함한다. Archivist가 실행한 Command도 별도 작업으로 숨기지 않고 Actor·requested_via·Tool Call 상관관계와 함께 같은 작업 상세에 표시한다. `COL-001~002`

화면은 무채색 표면·상태 신호 요소에만 쓰는 유채색·2px 이하 모서리·설명 문장 배제 원칙을 유지한다. Host·Session·Turn·ActionCard fixture는 실제 Chromium에서 운영체제별 PNG golden으로 검증하고, 의도된 변경만 golden을 갱신한다.

## 9. 배포와 보안

Archivist Host는 CLI를 실행할 PC마다 설치하고 Control로만 outbound 연결한다. 운영 Windows PC의 자체 loop와 fixture는 Windows Service를 사용할 수 있지만, Codex·Claude의 사용자 구독 로그인을 쓰는 Host는 해당 CLI에 로그인한 현재 console 사용자의 `InteractiveToken` 예약 작업으로 실행한다. Codex는 본체와 같은 버전의 code-mode host·exec server를 같은 디렉터리에 둔 전체 실행 번들만 허용한다. 설치 전에 이 번들과 같은 대화형 사용자 문맥의 CLI 로그인 probe를 통과해야 하며, Control의 Turn을 받은 Host가 그 사용자 Session에서 CLI process를 구조화 stdio로 연다. Windows Terminal 화면이나 ANSI 출력을 중계하지 않으며 다른 사용자의 profile에서 auth 파일을 복사하거나 `SYSTEM`·SSH batch logon에서 사용자 credential을 재사용하지 않는다. 현재 계정으로 직접 ChatGPT loop를 검증하는 macOS에서는 사용자 login session의 launch agent 또는 전경 process를 사용한다. Host PC에 inbound SSH나 HTTP 포트를 열지 않는다. `LRH-020`

Windows Credential Manager의 generic credential은 사용자 profile이 적재된 대화형 또는 전용 서비스 로그온에서는 사용할 수 있다. 그러나 OpenSSH 서비스 로그온처럼 Credential Manager가 사용할 로그온 세션이 없는 환경에서는 `CredWrite` 자체가 실패하므로 이를 Archivist Host의 유일한 secret 저장 경계로 두지 않는다.

이 경우 `dpapi://<absolute-blob-path>` reference를 사용한다. 설치·등록 도구는 credential을 `CRYPTPROTECT_LOCAL_MACHINE | CRYPTPROTECT_UI_FORBIDDEN`으로 보호해 임시 파일에 쓴 뒤 원자적으로 게시하고, 설치기는 blob 파일 ACL을 해당 Archivist Host 서비스 계정과 `SYSTEM`만 읽을 수 있게 제한한다. Resolver는 절대 경로만 허용하고 크기가 제한된 암호문을 읽어 메모리에서만 복호화한다. 평문은 boot 설정, argument, 환경 변수, 파일, 로그와 HTTP 응답에 넣지 않는다. machine scope는 profile 없는 서비스 프로세스 재시작을 허용하고, 계정 격리는 파일 ACL이 담당한다. `LRH-010`

macOS login session은 `keychain://<service>/<account>` reference를 사용한다. `archive-secret`은 평문을 stdin으로만 받아 login Keychain generic password를 만들고, Resolver는 reference로 값을 해석한다. ChatGPT 인증 Profile도 별도 Keychain item에 저장한다. OAuth는 system browser와 `127.0.0.1`의 `/auth/callback`만 사용하고 stable Host ID, issued client ID, 검증 identity와 rotating token set을 함께 보존한다. 최초 동적 등록 code가 `invalid_grant`이면 code는 폐기하고 callback의 issued client ID로 새 authorization을 한 번 수행한다. Profile 연결·해제는 해당 Mac의 `archive-archivist auth` CLI에서만 하며 아카이브 작업 공간은 원격 credential 변경 기능을 제공하지 않는다. `CFG-015`, `AGT-016~017`

Control boot 설정에는 다음만 사전 등록한다.

```text
archivist_host_id
credential_ref
enabled
allowed_backend_ids
allowed_cli_ids
allowed_workspace_root_ids
heartbeat_ttl
```

Host boot 설정은 Control URL, 같은 service credential reference, 실행 파일의 절대 경로와 고정 인수, workspace root ID→절대 경로 mapping을 가진다. 원격 Control에는 HTTPS 또는 승인된 암호화 overlay로 연결한다. 실행 명령은 executable과 argument array로 만들며 shell 문자열을 사용하지 않는다.

Worker와 Archivist Host는 같은 PC에 설치할 수 있지만 identity, credential, lease와 포트를 공유하지 않는다. Worker의 높은 CPU·GPU 사용은 CLI가 도구로 만든 Job의 실행 상태일 뿐, Archivist Host가 미디어 subprocess를 직접 실행하지 않는다.

## 10. v1 완료 시나리오

다음 acceptance가 통과할 때 아카이브 작업 공간에서 사람과 로컬 CLI Archivist가 같은 업무를 이어가는 v1 흐름이 완료된다.

1. ArchivistSession이 없는 빈 작업함에서 사람이 버튼·폼·API로 Job을 직접 만들고 작업 상세를 확인한다.
2. 사전 등록된 Windows Host와 CLI를 선택해 ArchivistSession을 만들고 같은 Job 문맥으로 ArchivistTurn을 제출한다.
3. 같은 ArchivistTurn ID가 Host의 로컬 CLI에 한 번만 전달되고 CLI가 MCP 조회 도구로 자산과 Job 상태를 읽는다.
4. 인증된 Archivist의 허용된 비파괴 Command는 공통 Command Bus에서 한 번 실행되어 사람이 만든 Event와 같은 작업 상세에 나타난다.
5. 승인 필요한 상태 변경은 ActionCard로 표시되고, 사람이 명시적으로 조작한 경우만 Approval 또는 Command가 만들어진다.
6. 사람이 API로 추가 변경한 현재 상태를 다음 ArchivistTurn이 다시 읽어 이어서 처리한다.
7. Host 연결과 CLI process를 강제로 끊고 다시 시작한다.
8. 같은 ArchivistSession이 재개되고 완료된 Tool Call과 Command를 중복 실행하지 않은 채 최종 응답을 끝낸다.

## 11. 구현 순서

1. Control의 ArchivistSession·ArchivistTurn·ArchivistTurnLease Core와 SQLite Store
2. Archivist Host 등록·heartbeat·claim·event stream API
3. Embedded Core backend와 fake CLI integration fixture
4. loopback MCP proxy와 Control MCP Adapter
5. 아카이브 작업 공간의 직접 Command·Session·Turn·ActionCard projection과 golden
6. 같은 Host의 local model fixture와 Archive MCP를 통한 subprocess 수직 조립
7. 실제 Windows Host에서 Codex 또는 Claude CLI acceptance
8. macOS Keychain과 실제 ChatGPT 계정 및 실제 자체 LLM으로 제한 도구 루프 acceptance
9. 필요할 때만 Orca CLI compatibility backend와 다계정 Provider Router

Orca CLI backend는 v1 완료의 선행 조건이 아니다. Embedded Core acceptance가 먼저 통과하고 Orca 연동의 유지비가 실제 운영 이점을 줄 때 추가한다.

## 12. 현재 구현 경계

Core에서 결정적으로 검증할 수 있는 계약은 구현 상태다.

- ArchivistSession Host 고정과 offline 대기 `LRH-002`
- ArchivistTurnLease 배타성·fencing `LRH-003`
- request 중복 전달 방지와 INPUT_ACCEPTED/STARTED 분리 `LRH-004`
- durable Session 기준 runtime rebind `LRH-005`
- native reference 우선과 최소 checkpoint `LRH-006`
- CLI·workspace allowlist 실행 계획 `LRH-007`
- runtime event cursor와 명시적 완료 `LRH-008`
- 정제된 진행·실패 항목의 cursor 멱등 저장·투영 `LRH-013`
- 참조 작업의 진단 이상과 새 연결 드라이브를 중복 없이 보고하는 자동 제안 Turn `LRH-023~024`
- Codex app-server native thread mapping·재개와 공개 이벤트 정제 `LRH-018`
- 승인 없는 native terminal·파일·Skill·보조 Agent와 정규화 진행 `LRH-019`
- Windows 대화형 사용자 Codex 로그인과 Host process Session 경계 `LRH-020`
- Tool Call→Command 상관관계와 receipt replay `MCP-002`
- Codex stdio MCP 업무 도구 allowlist와 Turn 상관관계 `MCP-006`
- 채팅과 ActionCard Approval 분리 `DSH-003`

Adapter 경계도 구현 상태다.

- Host service 인증·heartbeat·claim과 SQLite Session·Turn Store `LRH-001`, `CTL-005~006`
- loopback proxy와 별도 Control MCP listener의 versioned Capability allowlist `MCP-001`
- 고정 작업 진단 조회, 새 드라이브 제안의 선택 준비·실행 ActionCard 경계와 작업 이상 Turn의 증거 기반 직접 복구 `MCP-009~011`
- 사람·Archivist 공통 Command Bus와 정제된 아카이브 작업 공간 projection `COL-001~002`, `DSH-002`
- Archivist Runtime·ActionCard fixture의 Chromium PNG golden `DSH-004`

실제 Windows Archivist Host와 계약 fixture의 checkpoint 재개·중복 방지는 `LRH-011`, profile 없는 Windows 서비스 로그온의 DPAPI secret 경계는 `LRH-010` acceptance에서 검증했다. Codex app-server fixture의 native thread mapping·재개·정제 event 변환, 승인 없는 native 도구 정책과 Archive stdio MCP bridge는 `LRH-018~019`, `MCP-006`으로 검증한다. 실제 계정으로 인증된 Codex 또는 Claude CLI의 전체 협업·강제 재시작 증적은 `LRH-009`, 실제 공개 이벤트 정제 증적은 `LRH-014` acceptance가 남아 있다. fixture 결과를 제3자 CLI의 실계정 증적으로 대체하지 않는다.

macOS Keychain credential 왕복, ChatGPT OAuth Profile·token 회전, stateless streaming Responses 계약, loopback local Chat Completions Adapter, 제한 도구 루프와 실행 상한은 `CFG-015`, `AGT-016~021`로 구현됐다. Host process 계약에서 local model fixture가 Archive MCP 도구를 호출하고 receipt 뒤 최종 응답을 내는 수직 조립은 `LRH-016`으로 검증한다. 실제 macOS ChatGPT 계정의 읽기 도구 Turn은 `LRH-015`, 실제 자체 LLM의 읽기 도구 Turn은 `LRH-017` acceptance가 각각 소유한다. 현재 Mac 계정은 plan 사용 권한 동의 단계에서 차단되어 `LRH-015`를 planned로 유지하며, 실제 자체 LLM endpoint가 준비되기 전에는 `LRH-017`을 planned로 유지한다. 이 경계는 Codex·Claude의 native resume acceptance `LRH-009`, `LRH-014`를 대신하지 않으며 다계정 Router `AGT-001~015`도 구현됐다고 간주하지 않는다.

원본 탐색 누락은 드라이브 후보에서, QC 측정 실패와 후보별 배정 차단은 원래 작업의 진단에서 확인한다. 작업 미선택 상태에서 Control 모니터 장애·회복 및 최근 명령 호출은 전체 현황으로 확인한다. 원인 미분류나 미완료 호출에는 자동 복구 권한을 부여하지 않는다. `DEV-017`, `QCT-005`, `DSP-005`, `OBS-012~013`
