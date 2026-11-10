# 09. 아카이브 작업 공간 설계

## 1. 목적

아카이브 작업 공간(Archive Workspace)은 사용자가 상태를 보기만 하는 조회 화면이 아니다. 사용자와 Archivist가 같은 Job·Step·자산·Command·Event를 공유하면서 다음 행동을 함께 결정하고 실행하는 제품의 주 사용자 표면이다..

초기 아카이브 작업 공간은 다음 행동을 모두 지원해야 한다.

- 사용자가 버튼·폼 또는 API로 ArchivistSession 없이 새 Job과 도메인 명령을 직접 시작한다.
- 사용자가 채팅으로 ArchivistTurn을 만들고 Archivist에게 목표와 후속 지시를 전달한다.
- Archivist가 인증된 MCP allowlist Capability로 조회하고, ActionClass에 따라 Command를 실행하거나 ActionCard를 제안한다.
- 사용자와 Archivist가 만든 Command·Event·결과를 같은 작업 상세와 다음 ArchivistTurn 문맥에서 이어서 본다.
- 사용자가 실행 중인 Job·Step·ArchivistTurn에 개입하고 취소·재시도·승인 같은 후속 행동을 선택한다.

매체 소유권, 대여, 반환 승인과 반환 이력은 아카이브 작업 공간과 도메인 모델의 범위가 아니다. 작업 공간 드라이브의 lock과 안전 해제 상태는 Job 실행 상태이므로 표시한다.

## 2. 협업 진입 경로

```text
사용자 ── 버튼·폼·API ───────────────────────┐
                                             ├─ Command Bus ─> Job·Step·자산
사용자 ── 채팅 ─> ArchivistTurn ─> MCP Capability ┘
                                  │
                                  └─ ActionCard ─> 사람 Approval ─> Command Bus

Command·Event·실행 증적 ─> 공통 작업 상세 ─> 사용자 화면 + 다음 ArchivistTurn 문맥
```

사람의 직접 경로는 Archivist 가용성이나 ArchivistSession 존재 여부에 의존하지 않는다. 빈 시스템에서도 Template과 대상을 선택해 Job을 만들 수 있다. Archivist 경로도 별도 업무 원장을 만들지 않고 사람과 같은 Application Service 명령 경계, Command Bus, 리비전 검사와 멱등 receipt를 사용한다. 모든 영속 업무의 공통 Capability Registry 직접 실행은 `APP-001` planned 경계다.

채팅은 Archivist에게 내리는 지시다. “이 파일을 분석해”, “빈 행사 기록 작업을 시작해” 같은 지시는 ArchivistTurn으로 전달되고 Archivist가 허용된 도구를 호출할 수 있다. 다만 “승인”, “삭제해”, “포맷해” 같은 문장이 Approval이나 Confirmation 기록을 대신하지는 않는다.

요구사항: `COL-001`

## 3. 협업 연속성과 실행 경계

| ActionClass | 사람 직접 Command | Archivist MCP |
|---|---|---|
| `READ` | 허용 | allowlist 조회 |
| `PROCESS` | 허용 | 인증된 allowlist에서 직접 Command |
| `GATED` | 명시적 사람 결정 필요 | ActionCard만 제안 |
| `DESTRUCTIVE` | boot policy·Approval·Confirmation 필요 | ActionCard만 제안하고 직접 실행 금지 |
| `MANAGE` | 로컬 설정 파일과 재시작 | 원격 실행 금지 |

Job·Step·ArchivistSession에는 누가 다음에 조작할지를 고정하는 설정을 두지 않는다. 사람과 Archivist는 같은 작업 ID와 최신 revision을 사용하며 각 Command의 Actor와 requested_via로 실제 행위자를 기록한다. Archivist의 token·quota·인증·Host 문제가 생겨도 Worker 실행은 유지되고 사람 직접 경로로 같은 작업을 계속한다. 반대로 진행 중 작업을 넘길 때는 `JOB | ARCHIVE_RUN`, work ID, revision을 가진 `WorkReference`를 ArchivistTurn에 고정한다. Archivist는 `archive.work.reference.get.v1`으로 이 참조를 읽고 최신 Event와 receipt를 조회해 이어서 처리하며 채팅 본문에서 대상을 추론하지 않는다. `COL-001`, `COL-004`

ActionCard는 모든 Archivist 행동의 필수 중간 단계가 아니다. 승인 필요한 명령 후보만 기록한다. 허용된 일반 처리는 ArchivistToolCall과 Command receipt로 바로 추적한다.

요구사항: `SEC-001~003`, `MCP-002`, `DSH-003`, `COL-002`. 외부 Provider Router의 별도 allowlist 계약인 `AGT-012`는 planned다.

## 4. 공통 읽기 모델

작업 관리의 상위 행은 `ArchiveRun` 또는 그 실행에 속하지 않는 독립 `Job`이다. ArchiveRun 행은 `archive_run_id`, 고정 폴더 수, 영상 수, Worker, 원본·아카이브 Storage, 하위 Job 수와 현재 상태를 투영한다. ArchiveRun 상세는 검사·Folder·STT Job을 역할별 하위 실행으로 결합한다. 독립 Job과 하위 Job의 주소 가능한 상세는 다음 값을 투영한다.

```text
job_id, template_name, target_title
current_step_id, step_state, progress
executor_kind, actor_id, worker_id
archivist_host_id, archivist_session_id, archivist_turn_id, archivist_turn_state
archivist_action, tool_call_status, action_card_status
wait_reason 또는 error_class
last_event_at, log_reference
```

작업 상세는 Step DAG, CommandRecord, Event, Approval, ArchivistToolCall, ArchivistTurn, ActionCard, Worker progress와 정제된 process log 참조를 시간순으로 결합한다. 사람과 Archivist 중 누가 시작했는지를 Actor와 requested_via로 구분하되 별도 작업함으로 나누지 않는다. 읽기 모델은 원장이 아니며 SQLite 원장과 일시 metric에서 다시 만들 수 있어야 한다.

Worker 상태는 CPU, memory, GPU/VRAM, scratch 여유, heavy/light slot, 접근 가능한 Storage와 heartbeat를 보여준다. CPU·GPU·memory·scratch 원시 관측값과 활성 ResourceReservation을 구분하고, 예약 차감 뒤 남은 memory·scratch 및 사용/최대 slot을 WorkerCapacity로 함께 투영한다. v1 기본 heavy/light Step을 하나 더 받을 수 있으면 `추가 가능`, 받을 수 없으면 실제 `WaitReason`을 명사형 상태로 표시한다. 이 기준 상태는 비교용이며 큐의 실제 Step은 그 Step에 저장된 예상량으로 같은 Scheduler를 다시 통과한다. 장치 정보는 Job을 실행할 수 있는지, 논리 Volume 작업 공간의 선택과 별도 PhysicalDevice I/O lock 충돌·안전 제거 가능 여부를 설명하는 범위에서만 보여준다. `DSH-014`

Archivist Session 영역은 Host 상태, backend, CLI, workspace label, 현재 Turn 상태와 마지막 event 시각을 표시한다. credential reference, native resume token과 raw terminal screen은 표시하지 않는다. `DSH-001~002`

초기 웹 표면은 Archive Control에 embed되어 별도 frontend build 없이 제공된다. 메인 아카이브 작업 공간은 드라이브 선택·아카이브 실행·Archivist 대화를 제공하고 자산 metadata·전사 timecode 검색은 별도 “아카이브” 화면으로 이동한다. 두 화면은 rebuild 가능한 같은 SQLite projection을 읽는다. 네트워크 접근 판정은 정적 파일과 API에 동일하게 적용되고 원본·아티팩트의 Storage 경로는 조회 DTO에 포함하지 않는다. `CTL-001`

허용된 내부 CIDR의 평문 HTTP 접속은 브라우저 secure context가 아닐 수 있다. 따라서 초기 렌더와 브라우저 명령 ID 생성은 `crypto.randomUUID` 같은 secure-context 전용 API의 존재를 전제로 하지 않고, 사용할 수 없는 경우 `crypto.getRandomValues`로 같은 UUID v4 형식을 만든다. `CTL-001`, `DSH-009`

## 5. 사용자 직접 명령과 Archivist 협업

사용자는 작업함이 비어 있어도 Template 기반 Job을 만들고 허용된 도메인 API를 직접 호출할 수 있다. 이 경로는 Archivist 부재 시 폴백이 아니라 정상적인 첫 번째 실행 경로다.

- create, pause, resume, cancel, retry
- 현재 작업 ID·revision을 유지한 채 사람 또는 Archivist가 다음 명령을 제출
- 입력·metadata·검수 결과 기록
- 승인 대상과 리비전을 확인한 뒤 Approval 기록
- 같은 Command Bus를 통한 허용된 도메인 기능 실행

설정의 Archivist 추가는 CSRF 검증 뒤 이름·사전 등록된 Archivist Host·감지된 CLI만 받는다. backend와 workspace는 Host 시작 설정이 결정하며 Archivist Host 등록 자체도 설정 파일과 Host 설치가 소유한다. 채팅 제출은 ArchivistTurn을 먼저 저장하고 durable Session·Turn ID를 반환한다. 화면에서 고른 Worker·원본·아카이브 Storage·폴더 범위는 ID와 revision으로 Turn에 붙이며 본문에서 추론하지 않는다. 새 Turn에 선택 범위나 작업 참조가 없다는 조회 결과는 정상적인 `bound=false` 문맥이고, 실행 명령만 고정 선택을 요구한다. `CTL-003~006`, `COL-003`, `DSH-012`, `MCP-008`

ActionCard의 채팅 분리는 승인 경계에만 적용한다. 채팅의 “진행”은 Archivist에게 후속 행동을 요청할 수 있지만 그 자체를 Approval 또는 ActionCard 제출로 해석하지 않는다. 명시적 카드 조작 시 현재 리비전을 다시 검사한다. `DSH-003`

Archivist가 제안한 `GATED`·`DESTRUCTIVE` ActionCard는 SQLite에 먼저 저장하고 아카이브 작업 공간에 `PENDING`으로 투영한다. 사용자의 명시적 카드 실행·거절 API는 일반 브라우저 명령과 같은 Origin·JSON·CSRF 검사를 통과한 뒤 `PENDING → EXECUTING` compare-and-swap으로 카드를 먼저 선점한다. 한 요청만 선점에 성공하므로 실행과 거절이 경합해도 둘 다 종결될 수 없다. 실행은 카드에 고정된 Command·parameter·expected revision을 공통 Command Bus로 제출하고, ArchiveRun을 시작한 카드라면 카드가 속한 Session·Turn에 같은 ArchiveRun 업무 문맥도 묶은 뒤 `COMPLETED`로 바꾼다. 실패하면 안전하게 `PENDING`으로 돌리고 같은 멱등 receipt로 재시도한다. 거절은 actor·시각·사유가 있는 immutable 기록과 `REJECTED` projection만 남기며 Command를 만들지 않는다. `COMPLETED`·`REJECTED` 카드는 다시 조작할 수 없다. `DSH-005`

초기 웹 Command API는 CSRF session을 먼저 발급하고 exact Origin·JSON Content-Type·double-submit token을 검증한다. 이미 유효한 CSRF cookie가 있는 브라우저는 같은 token을 재사용하므로 두 번째 탭이 첫 탭의 조작을 무효화하지 않는다. Template 기반 Job 생성은 revision 0에서 새 Aggregate를 같은 Command Unit of Work로 만들며, 취소는 요청의 expected revision과 StepState 전이표를 모두 통과해야 한다. 미시작 Step은 즉시 정산하고 실행 중 Step은 별도 CancellationRequest와 Worker stop·정리 확인을 거친다. 성공 뒤 rebuild 가능한 projection을 갱신하고 멱등 재전송에서는 최초 명령 결과로 같은 투영을 복구한다. `CTL-003~004`, `CAN-001~005`

Provider, Worker registry, Archivist Host, 인증, Storage와 파괴적 기능 정책은 v1 아카이브 작업 공간이나 API에서 바꾸지 않는다. OS ACL로 보호된 로컬 설정 파일과 서비스 재시작이 그 책임을 가진다. `CFG-001~002`, `SEC-004`

## 6. 상태 전달

- 저장된 Event 기반 상태는 SSE reconnect cursor 이후 재생한다. `NTF-001~002`
- 진행률과 자원 metric은 live 상태이며 재연결 때 과거 값을 재생하지 않는다.
- 사람과 Archivist가 만든 Command 결과를 같은 작업 상세에 반영한다.
- 다음 ArchivistTurn 문맥은 현재 Job·Step 리비전과 완료 Tool Call receipt를 다시 읽어 구성한다.
- 로그 원문은 파일에 회전·압축하고 아카이브 작업 공간에는 정제된 tail과 참조만 전달한다.
- 현재 Notification Feed에는 아카이브 실행 event 발행자가 없다. 아카이브 작업 공간은 화면이 보이는 동안 5초마다 Worker·드라이브·Host·Archivist·Job 읽기 모델을 다시 읽는다. 드라이브 목록·상세·알림·실행 gate는 각기 상태를 추측하지 않고 드라이브 projection의 같은 `VolumeConnectionState`와 관측 시각을 사용한다. API 갱신 자체가 세 번 연속 실패하면 본문 위에 연결 끊김과 마지막 갱신 시각을 한 줄로 보이며 회복될 때까지 판정·실행·취소 Command를 잠근다. 펼친 보류 판정과 선택 상태는 다시 읽어도 유지한다. `DSH-021`, `DSH-026`
- route가 바뀌면 이전 화면의 진행 중 fetch를 취소하고 dispose된 view의 늦은 응답은 DOM을 갱신하지 않는다. 같은 상태 변경 endpoint는 첫 요청이 끝날 때까지 다시 제출하지 않아 빠른 이중 클릭이 새 idempotency key의 중복 Command를 만들지 않는다.
- Archivist Host 상태와 ArchivistTurn event는 저장 cursor 이후 replay하며 raw PTY output을 업무 상태로 취급하지 않는다.

## 7. 화면 계약

연결 관리의 논리 드라이브 행은 현재 연결·Worker·단계·아카이브를 함께 보인다. `CONNECTED`는 연결됨, online Worker의 현재 관측에서 빠진 영속 드라이브는 연결 해제됨, Worker 자체가 offline이면 확인 불가로 표시한다. 연결 해제는 알림과 주소 가능한 드라이브 상세에도 같은 값으로 나타나고 작업 관리의 새 작업 원본 후보에서 제외되지만 완료 실행·Package·Catalog 이력은 지우지 않는다. `DEV-010`, `DSH-010`, `DSH-026`

상태 신호에서 `DISCONNECTED`는 실패, `CONNECTED`는 완료, 연결을 관측할 수 없는 `UNKNOWN`은 중립이다. 드라이브의 아카이브 진행 상태와 연결 상태는 서로 덮어쓰지 않고 별도 항목으로 함께 표시한다.

메뉴 분리와 기능 수준별 화면 재구성은 [14. 아카이브 작업 공간 UX·UI 설계 기준](14-archive-workspace-ux.md)의 확정 결정을 따른다. 화면 틀은 왼쪽 사이드바, 본문, 오른쪽 Archivist 대화 칸의 세 자리이고 상단 막대는 두지 않는다(결정 9-2, 결정 10~19). 사이드바 맨 위에는 기록물·전사문 검색 칸과 모든 대상의 실패와 사람 대기를 하나씩 보여 주는 알림 버튼을, 아래 끝에는 설정을 두고 테마 전환은 설정 화면 안에 둔다. 사이드바 메뉴는 작업 관리·연결 관리의 두 route이고 작업 관리가 첫 메뉴이자 기본 화면이다. 메뉴에는 개수를 붙이지 않는다. 메뉴 아래에는 미완료 관리 작업을 이름·상태만 보이는 카드 하나씩 표시하는 스크롤 가능한 워크스페이스 목록을 두고 작업 ID·드라이브·Worker는 카드의 마우스 오버로 보인다. 완료 관리 작업은 이 목록에서 제거하고 작업 관리의 완료 목록과 영속 상세에만 유지한다. 아카이브 접수는 선택한 폴더 수와 무관하게 ArchiveRun 카드 하나이며 검사·Folder·STT Job은 그 실행 상세의 하위 작업이다. ArchiveRun을 참조하지 않는 Template Job만 독립 카드로 같은 `job_id` 상세에 이동한다. 이 카드는 새 aggregate를 만들지 않는다. 지금 위치는 각 상세 화면 제목 위의 경로 줄이 보인다. 작업 관리는 최상단의 `새 작업 생성`에서 아카이브 또는 독립 Template 작업을 고른다. 아카이브는 연결 원본·아카이브 저장소·READY 폴더 범위를 선택해 ArchiveRun을 만들고 그 상세로, Template는 이름·Template을 기존 Job Command Bus에 제출해 `job_id` 상세로 이동하며, 이어서 실행 중 ArchiveRun과 독립 Job, 자원 대기·대기열 관리 작업의 작업 큐, 실패·사람 입력 대기 관리 작업의 확인 필요 목록, 처음에는 접힌 끝난 관리 작업의 완료 목록을 둔다. 확인 필요와 완료 목록은 각각 접고 펼칠 수 있게 둔다. 작업 행 하나는 사람이 요청한 작업 하나이며 원본 드라이브 이름(독립 Job은 작업 이름), 첫 폴더와 나머지 폴더 수·영상 수(독립 Job은 Template 종류), 흐리고 작게 쓴 작업 ID, 상태와 요청 시각을 보이고 진행률은 실행 중 행에만, Worker는 마우스 오버로 둔다. 사람은 작업을 ID로 구분하지 않으므로 ID는 식별의 주 정보가 아니다. 행을 누르면 그 자리에서 ArchiveRun의 고정 범위 폴더별 상태 행이나 독립 Job의 Step 행과 주소 가능한 작업 화면으로 가는 `작업 화면 열기`를 펼친다. 작업 큐는 별도 원장이 아니라 저장된 ArchiveRun·Job·Step의 projection이다. ArchiveRun 상세(화면 표기는 작업 화면)는 원본 드라이브 이름 아래 첫 폴더·요청 시각·흐린 ID를 두고, 그 드라이브의 현재 실행이면 드라이브 상세와 같은 개요와 확인 필요 패널을 두어 보류 판정 같은 해결 조작을 그 자리에서 제공하며 지금 처리 중인 폴더 작업을 진행 중 목록과 같은 행으로 보인다. 원본→아카이브·폴더 수·영상 수를 보이며 identity·Worker·하위 작업 수·결과 수는 마우스 오버로, 고정 폴더 범위는 처음에 접힌 목록으로 두고 하위 Job·ArchivePackage 완료 결과를 결합한다. 하위 Job은 주소 가능한 Job 상세와 취소 경계를 유지한다. 완료 Job의 `아카이브 결과` 조작과 드라이브 완료 행은 같은 ArchiveRun 상세의 정확한 Package 결과를 선택하며 Storage root가 아닌 검증된 패키지 상대경로를 보여준다. 연결 관리는 Worker PC·논리 드라이브·Agent와 Agent Host를 한 화면의 독립 영역으로 모으고 Worker 자원·Storage 접근, 드라이브의 Worker·단계·아카이브, Agent의 Host·CLI를 보인다. 드라이브 행은 주소 가능한 드라이브 상세로 이동한다. 검색은 메뉴가 아니라 사이드바 위의 검색 칸에서 연 결과 화면이다. Archivist는 모든 route의 오른쪽에 늘 열린 대화 칸으로 두고 화면에는 Agent로 표기한다. 고정 fixture를 실제 Chromium viewport로 렌더링한 운영체제별 PNG golden과 폭별 배치 검사로 검증한다. `DSH-004`, `DSH-007`, `DSH-009`, `DSH-014`, `DSH-016`, `DSH-019`, `DSH-024~026`, `DSH-028`, `DSH-032~035`

ArchiveRun과 Job은 펼침 패널의 임시 데이터가 아니라 각각 `archive_run_id`와 `job_id`를 가진 영속 객체다. 작업 관리와 왼쪽 워크스페이스는 ArchiveRun 및 독립 Job의 관리 작업 projection을 사용하되 왼쪽 워크스페이스는 완료 상태를 제외한다. 작업 관리에서 펼친 ArchiveRun 행의 `작업 화면 열기`는 `#/runs/<archive_run_id>` 상세로, 드라이브 완료 내역과 완료 Job의 아카이브 결과 참조는 `#/runs/<archive_run_id>?result=<archive_package_id>`로 이동하고, 그 상세의 하위 실행은 `#/jobs/<job_id>`로 이동한다. ArchiveRun 상세는 고정 폴더 범위와 검사·Folder·STT Job 및 ArchiveCompletionEntry를 결합하고 Storage root 대신 Package 상대경로를 포함한다. Job 상세는 최신 리비전·StepState·WaitReason·진행·Worker를 보존한다. 두 projection은 원장이 아니므로 Control 시작 시 영속 ArchiveRun과 Job aggregate에서 재구성한다. 이전 버전에서 시작해 ArchiveRun aggregate snapshot은 없지만 현재 드라이브 projection과 archive_run_id가 남은 실행은 그 드라이브의 고정 범위·Worker·Storage와 연결 Job으로 ArchiveRun 상위 작업을 복원하며 내부 Job을 독립 작업으로 펼치지 않는다. `DSH-001`, `DSH-016`, `DSH-019`, `DSH-024~025`, `DSH-028~029`, `CTL-007`

ArchiveRun 하위 작업과 Job 상세의 영상 정보는 초기 상세 응답에 포함하지 않는다. 담당자가 특정 Job의 영상 정보 disclosure를 펼칠 때 `job_id` 전용 읽기 API를 한 번 호출하고, 접으면 영상 행을 화면에서 제거하며 같은 화면에서 다시 펼칠 때는 받은 응답을 재사용한다. 읽기 모델은 그 Job의 영속 `StepExecutionSpec.Parameters.SourceInputs`에서 원본 자산별 입력을 중복 제거하고 같은 ArchiveRun의 현재 드라이브 projection에 영속된 Folder별 분류 관찰값을 동일 OriginalAsset ID 또는 상대경로로 결합해 비어 있는 기술정보를 보강한다. 원격 검사 Job처럼 실행 사양에 파일 입력을 직접 싣지 않는 경우에는 그 분류 관찰값을 그대로 사용한다. 원본 생성일·원본 이름을 먼저, 상대경로·길이·용량·container·video/audio codec·해상도·bitrate·Profile을 뒤에 두며 값이 없으면 추정하지 않고 Storage root와 mount path를 내리지 않는다. `DSH-030~031`

StepState `WAITING`은 이유를 포함하지 않는다. 드라이브 워크스테이션은 각 Stage에 저장된 WaitReason을 함께 투영한다. `FAILED`는 실패, `INPUT`·`APPROVAL`·`DEVICE`는 확인 필요로 드라이브 상세의 확인 필요 칸과 알림 목록에 올리고 `MEMORY`·`SCRATCH`·`SLOT`·`DISK`·`IDLE`·`PRIORITY`는 사람이 할 일이 없는 진행 상태로 둔다. `INPUT`은 사람 또는 허용된 Archivist의 명시적 입력을 기다리며, 화면은 담당자가 직접 처리할 수도 있으므로 결정 신호를 유지한다. 폴더 표의 행은 상태 아이콘·폴더 이름과 한 단어 상태 또는 보관됨/확정됨만 둔다. 처리 방식·영상 수는 행의 마우스 오버와 폴더 세부에서 보인다. 한 단어 상태는 실패와 명시적 조치 대기만 `실패`, `분류 확인 필요`, `저장소 연결 대기`처럼 드러내고, 정상 진행·자원 대기는 `진행 중`·`대기열`·`완료`로만 둔다. 단계 이름·완료 수/전체 수·대기 이유는 폴더 세부의 `세부 단계 보기`를 펼칠 때 `단계 · 완료 수/전체 수 · 이유 대기` 형태로 보인다. 확인 필요 칸은 실패한 단계와 완료 수, `DEVICE`의 Worker 결과에 저장된 대상 Storage ID처럼 해결에 필요한 값만 보이며 단계나 파일명에서 추측하지 않는다. WaitReason이나 대상이 없으면 상태·단계 ID·Disposition에서 추론하지 않는다. `DSH-007~008`, `MCP-004~005`

ArchivistTurnState `WAITING`은 StepState와 다른 상태 집합이며 현재 별도 이유 값이 없다. 따라서 ArchivistTurn WAITING은 INPUT·APPROVAL·재연결 중 무엇인지 추정하지 않고 중립 신호로 표시한다. 향후 ArchivistTurn 대기 이유를 모델링할 때는 Step WaitReason을 재사용하지 않고 별도 요구사항과 상태 계약으로 추가한다. `DSH-004`

상태 표시는 원래 상태 값마다 색을 두지 않고 다음 행동 주체 기준의 다섯 신호로 묶는다. 실패 신호는 `FAILED`와 heartbeat가 끊긴 `OFFLINE`, 결정 신호는 명시적 조치 WaitReason을 가진 `WAITING`·HOLD·대기 중 ActionCard·사람 개입이 필요한 관리 작업, 처리 신호는 `RESERVED`·`RUNNING`·`VERIFYING`·`BUSY`·`CONNECTING`·`LEASED`·`SUBMITTED`, 완료 신호는 `COMPLETED`·`CONNECTED`·`ONLINE`이며 나머지는 중립 신호다. 화면이 동시에 쓰는 유채색은 실패·결정·완료의 세 가지로 제한하고, 세 색은 적색맹·녹색맹 시뮬레이션에서 서로 구분되며 패널 배경 대비 3:1 이상인 값으로 고른다. 처리 신호는 사람이 할 일이 없는 상태이므로 결정 신호와 경쟁하지 않도록 무채색으로 둔다. 유채색은 신호 점·단계 점·배지·진행 막대·짧은 신호 label에만 쓰고 표면·본문 글자·테두리 token은 무채색으로 유지한다. 색만으로 의미를 전달하지 않도록 상태 아이콘은 색에 모양과 기호를 더한다. 실패는 ✕가 든 원, 확인 필요는 !가 든 마름모, 처리는 도는 호, 완료는 ✓가 든 원, 시작 전은 빈 원이다. 사이드바 위의 알림 버튼은 선택한 화면과 관계없이 모든 드라이브·Worker·Host·독립 Job과 대기 중 ActionCard를 projection에서 세어 실패·확인 필요 합계를 보이고 문제 하나마다 드라이브·폴더 이름, 문제 종류와 해결 위치를 보인다. 알림 항목은 해당 드라이브 상세·연결 관리·Agent 카드 또는 작업 상세로 이동한다. 정상 진행 중인 폴더 상태는 드라이브 상세 밖에 드러내지 않는다. 작업 관리에는 ArchiveRun과 독립 Job만 상위 행으로 두며, 실행 중은 최상단에, 자원 대기·대기열은 작업 큐에, 실패·사람 입력 대기는 확인 필요 목록에, 끝난 항목은 완료 목록에 둔다. ArchiveRun 내부 Job은 상위 목록에 중복하지 않는다. 드라이브 상세의 개요 카드는 단계·보관 진행 사실과 1단계 정리·2단계 전사·기록 세 단계 요약을 유지하고 연결선 없는 단계 상태 행렬은 펼칠 때만 보인다. 화면을 연 뒤 새로 생긴 실패·확인 필요는 알림 목록에 새로 표시하고 본문 아래쪽 토스트로도 잠시 보이며, 토스트의 열기를 누르면 같은 해결 위치로 이동하고 닫으면 사라진다. 화면 밖 알림은 두지 않는다. `DSH-004`, `DSH-007`, `DSH-019`, `DSH-020`, `DSH-026`, `DSH-028`

완료된 transcript Step의 출력 기록에 `TranscriptQualityAssessment.REVIEW_RECOMMENDED`가 있으면 해당 폴더를 확인 필요 신호로 유지한다. 아카이브 실행과 STT Job은 완료로 남으며 폴더 행과 확인 필요 칸에는 `전사 검토 권장`, 펼친 칸에는 Transcript ID, 저장된 고정 사유와 길이·구간 수·문자 수를 표시한다. `PASSED`는 정상 완료 표시를 추가하지 않는다. 화면은 전사문·파일명·구간값으로 품질을 다시 추론하지 않는다. `DSH-018`

Provider 계정의 인증 실패는 별도 수신 역할에 보내는 알림으로 표현하지 않는다. `AUTH_FAILED`와 재연결 필요 상태를 확인 필요 목록에 계정 alias와 함께 투영하는 계약은 `DSH-015`이며 Provider runtime projection이 연결될 때까지 계획 상태다.

드라이브 목록은 원본 드라이브를 아카이브 실행이 없음·끝나지 않음·완료의 실행 전·처리 중·완료로 묶어 카드로 보이고, 카드에는 단계·보관 영상 비율과 실패·확인 필요 수만 둔다. Worker·아카이브 저장소·영상 수는 마우스 오버로 보이고 꺼진 Worker만 카드에 남긴다. 폴더 상태는 드라이브 상세 밖에 드러내지 않는다. 새 아카이브 작업의 실행 준비는 작업 관리에서 로컬·연결 네트워크·새 외장 드라이브·FTP·SFTP 아카이브 후보의 역할·용량을 표시한다. `StorageAccessKind`는 각각 로컬 저장소·네트워크 드라이브·새 드라이브·FTP·SFTP로 표시하고 접근 불가 후보는 선택할 수 없게 둔다. SMTP는 이 목록에 넣지 않는다. Storage 역할·접근 방식·여유 용량은 Worker Storage projection이 제공할 때만 표시하고 Storage ID 문자열에서 추론하지 않는다(`DEV-010`). 아카이브 후보는 그 드라이브 Worker가 접근하는 Storage 중 원본과 다른 것이며 담당자가 직접 고르기 전에는 선택된 후보가 없다.

드라이브 상세는 원본 드라이브 하나의 inventory와 이력을 보는 자리다. ArchiveRun이 없어도 READY inventory의 폴더 구조를 표시하고, 폴더를 펼치면 그 폴더의 영상별 파일명·root-relative 경로·크기·filesystem 수정 시각을 보여 준다. 영상 행은 펼치기 전에는 만들지 않으며 절대 root와 mount path는 내리지 않는다. 새 ArchiveRun의 원본·아카이브 저장소·폴더 범위 선택과 생성은 작업 관리에만 있고 드라이브 상세에는 실행 준비·범위 체크박스·시작 조작이 없다. 기존 실행이 있으면 개요 카드에 단계·보관 진행 사실을 두고 확인 필요와 완료 내역을 유지한다. 제목 옆에는 기존 ArchiveRun이 STT 시작 가능할 때의 STT gate와, 기존 실행이 있으면 그 작업 화면으로 가는 `작업 열기`, 실행 전 연결 드라이브에는 작업 관리의 새 작업 생성을 이 드라이브가 고른 상태로 여는 `작업 만들기` 이동만 둔다. 실패한 폴더는 그 자리에서 다시 실행하지 않는다. 실패 패널의 `새 작업으로 다시 요청`은 이 드라이브·그 폴더·원래 아카이브 저장소를 고른 작업 관리의 새 작업 생성을 열고, 실행이 끝나지 않은 드라이브에서는 끝날 때까지 비활성이며 이유를 함께 보인다. 그 아래 확인 필요·완료 내역·폴더 구조를 둔다. 완료 내역은 실제로 완료된 1단계 Folder Job의 `ORGANIZED_MEDIA` 출력 하나를 행 하나로 하며 결과 영상명·원본 폴더·보관됨/확정됨을 보이고, 처리 방식·원본 수에서 결과 하나로 이어지는 수치·완료 시각·아카이브 Storage·Package 상대경로는 마우스 오버와 그 결과의 ArchiveRun 상세에서 보인다. 행을 누르면 같은 ArchiveRun 상세의 Package 결과가 선택된다. 최신 ArchiveRun이 바뀌어도 이전 행을 유지하고, 실행 상태가 완료여도 HOLD·실패처럼 ArchivePackage 출력이 없으면 행을 만들지 않는다. Worker가 꺼져 있으면 Worker 사실을 실패 신호로 표시하고 마지막 응답 시각은 마우스 오버로 보인다. 확인 필요 칸은 실패를 먼저, 알림에서 온 폴더를 펼친 채로 연다. 보류 폴더는 서버가 구조화된 입력 근거로 계산한 실행 가능 작업만 선택지로 보이고, `작업 선택`을 눌러야 명시적 override Command를 보낸다. 자동 조건을 만족하지 않는 그대로 보관이나 실제 renderer가 처리할 수 없는 소스 시퀀스는 선택지에 넣지 않는다. 보관됨과 확정됨을 합치지 않는다. 보관 결과의 주 표기는 처리 날짜와 원본에서 만든 결과 영상명이며 opaque ArchiveRun·Job·Package ID는 title의 추적 보조 정보로만 남긴다. `ORG-003`, `ORG-012`, `DSH-006~008`, `DSH-010`, `DSH-013`, `DSH-019`, `DSH-022~025`, `DSH-033`

Archivist 대화 칸은 화면에 Agent로 표기하며 Orca native chat과 같은 웹 셸형 채팅으로 사용한다. 로컬 CLI의 raw ANSI 화면을 복제하지 않고 선택한 Session 이름과 실제 CLI ID, 사용 가능·처리 중·Host 꺼짐·Archivist 없음 중 하나의 상태를 보이며, Archivist Host는 마우스 오버로 보이고 사용자·Archivist·SYSTEM Turn, 정제된 ArchivistTurnEntry, Archivist 응답 요약과 결과 ID, Tool Call 기록을 표시한다. 입력은 Enter로 전송하고 Shift+Enter만 줄바꿈하며 IME 조합을 확정하는 Enter는 전송으로 처리하지 않는다. Codex의 native terminal·파일·Skill·보조 Agent 작업도 같은 Turn 안에서 수행하되 화면에는 명령문·출력·diff·하위 Agent prompt가 아니라 정규화된 진행 이름만 표시한다. ArchiveRunWorkContext가 같은 최초 Turn과 SYSTEM 재호출 Turn은 하나의 업무로 묶는다. 업무는 사람의 지시와 최신 Archivist 응답, 결정 대기 ActionCard만 펼쳐 두고, 실행 중이면 진행 중 Turn의 마지막 진행 한 줄을 함께 보인다. 정제된 진행·재호출·Tool Call·이전 응답·처리된 ActionCard·결과 ID는 `작업 내역 · 도구 N · 재호출 N`의 접힌 영역에 보존하고, 전체 완료 재호출 Turn이 terminal 상태가 되면 최초 지시와 최종 응답 또는 최종 실패만 남긴 채 접힌 영역에 소요 시간을 더한다. raw terminal 화면·stdout·stderr·비공개 추론은 저장하거나 표시하지 않는다. 사용자 Turn에는 그 Turn에 고정된 DriveSelection과 WorkReference를 `참조 N`의 접힌 줄로 붙여 과거 지시의 대상을 남기고, Job 또는 ArchiveRun 상세에서 보내는 입력에도 현재 work ID·revision을 자동 고정한다. ActionCard는 카드를 요청한 Turn의 업무 문맥에 끼운다. 완료·거절 카드는 접힌 영역에 보존하지만 `PENDING` ActionCard는 접힘 밖에 남겨 조작 가능하게 한다. 현재 생성하는 비승인 카드는 `실행` 또는 `거절`을 자체 결정 기록으로 남기고 두 결정을 혼합하지 않는다. GATED 카드의 `승인`은 아카이브 확정 오케스트레이션과 함께 `ARC-012`에서 활성화한다. 선택 요청 카드는 실행 대신 작업 관리의 새 작업 생성을 연다. 알림의 Agent 결정 대기 항목은 대화 칸에서 그 카드를 강조한다. Host가 offline이거나 Archivist가 Turn 처리 중이면 입력을 비활성화하고 저장된 이유를 표시한다. 구조화된 참조 칩은 Turn 참조를 보여줄 뿐 본문 문구를 선택으로 해석하지 않는다. 설정의 Archivist 추가 화면은 Host 연결 상태를 함께 보여주고 감지된 CLI가 없는 Host에서는 CLI 선택과 추가를 비활성화한다. `DSH-011`, `DSH-012`, `COL-004`, `LRH-012~014`, `LRH-018~019`

화면 폭이 바뀌어도 현재 route의 조작 순서를 보존하고 Archivist 대화 칸은 늘 오른쪽에 둔다. 761px 이상에서는 사이드바가 본문 왼쪽 전체 높이(1280px 이상 216px, 그보다 좁으면 200px)를 차지하고, 1024px 이상에서는 대화 칸이 본문 오른쪽 전체 높이(1280px 이상 340px, 그보다 좁으면 300px)에 8px 안쪽으로 떠 있으며 본문과 대화 기록이 각자 scroll한다. 1024px 미만에서는 오른쪽 가장자리 손잡이로 여는 대화 칸이 viewport 오른쪽에 전체 높이로 열리고, 760px 이하에서는 사이드바가 본문 위 한 줄이 된다. 대화 기록은 칸 안에서만 scroll한다. 넓은 표와 실행 준비 선택 영역은 문서 전체가 아니라 자기 영역 안에서만 이동한다. 기준 PNG golden과 390·820·1024·1440·2560px에서 문서 가로 넘침, 메뉴 줄바꿈, 개요→확인 필요→폴더 순서와 사이드바·대화 칸 위치를 검사한다. `DSH-009`

golden은 드라이브 상세의 실행 전·1단계 진행·STT 가능·완료, 관리 작업 목록과 ArchiveRun 상세의 여섯 장면을 장면별·운영체제별 PNG로 승인한다. 크기와 의미 있는 pixel은 정확히 일치해야 한다. 같은 Windows Chromium 실행이 만드는 16 pixel 이하, RGBA channel delta 2 이하의 모서리 raster 오차만 비교에서 제외한다. 이 한계를 넘는 차이는 항상 actual·diff 증적을 남기고 실패한다. 갱신 명령은 지금 실행 중인 운영체제의 golden만 쓰고 다른 운영체제 이름으로 저장하지 않는다. Linux·Windows golden은 해당 운영체제 runner가 남긴 실제 렌더를 검토해 반영한다. `make archive-workspace-golden-evidence`는 `DSH-004`, `DSH-007`, `DSH-009~013`, `DSH-025`, `DSH-033` 테스트 결과, 세 운영체제의 승인 golden 18개와 작업 트리 변경 hash를 함께 기록한다. 2026-11-03 이전 산출물은 접수·작업·검색·장비 화면의 회귀 자료일 뿐 현재 증적이 아니다.

화면은 기본으로 어두운 무채색 테마를 쓰고, 설정 화면의 테마 전환으로 밝은 무채색 테마를 고를 수 있다. 테마 선택은 그 브라우저에만 남는 편의 설정이다. 두 테마 모두 표면 명도 차와 hairline 경계로 깊이를 구분하고, 본문 글자는 순흑·순백이 아닌 짙은 회색과 밝은 회색으로 배경 대비를 줄인다. 라틴 글자와 숫자는 내장한 Geist, 한글은 내장한 Noto Sans KR로 그린다. 일반 표면과 조작의 모서리는 2px 이하로 날카롭게 둔다. Agent 대화 칸과 그 안의 말풍선·결정 카드·입력은 대화를 작업 표면과 떼어 보이도록 둥글게 둔다. 목록은 하나의 테두리 묶음으로 그리고 항목 사이만 같은 선으로 나누며, 상태 신호 밖의 표면·본문·테두리에 유채색을 쓰지 않는다. 본문과 조작 글자는 12px 미만을 쓰지 않고 보조 글자는 배경 대비 4.5:1, 중립 도형은 3:1 이상으로 둔다. 밀도는 글자를 줄이지 않고 표·내부 scroll·세부 패널로 확보하며 큰 제목이나 넓은 여백보다 대량 작업 상태의 비교를 우선한다. 화면 글자는 한국어 명사형 label로 쓰고 원래 상태 이름은 title과 접근성 이름에 보존한다. 본문에는 기능을 서술하는 안내 문장을 두지 않으며, 짧은 명사형 label·업무 데이터·상태 token·조작 요소의 배치만으로 흐름을 드러낸다. 판단과 조작에 바로 쓰지 않는 보조 값(요약 수, Worker·아카이브 경로, 영상·폴더 수, 고정 범위, CSV 기록, 예약량, 작업 ID와 오류 종류 같은 세부)은 화면에 두지 않고 그 대상에 마우스를 올렸을 때 title로만 보인다. 꺼진 Worker처럼 문제를 알리는 값은 화면에 남긴다. Archivist 응답은 대화 내용이므로 이 규칙의 대상이 아니다. 접근성 이름과 live status는 시각 문구와 별개로 보존한다.

Worker의 전체 탐색 폴더 목록과 ArchiveRun의 고정 폴더 범위는 별도 projection 필드로 유지한다. 실행 상세의 폴더 표에는 영속 생성 시각이 가장 최근인 ArchiveRun을 검사 receipt가 생기기 전부터 즉시 선택하고, receipt가 생긴 뒤에는 그 고정 범위와 그 실행에 속한 판정·Job만 표시하며 이전 실행의 같은 input revision이나 같은 폴더 판정을 섞지 않는다. 완료 내역은 이 폴더 표와 다른 projection이며 모든 ArchiveRun의 검증된 ArchivePackage 출력을 완료 시각 역순으로 유지한다. 실행 완료 뒤 새 접수에서는 최신 전체 탐색 목록을 다시 사용하고, 새 작업 생성은 현재 candidate revision·Worker·source Storage가 모두 일치하는 선택 세션만 사용한다. 과거 ArchiveRun의 접수 세션 ID는 실행 이력에 남지만 새 작업 선택 세션으로 재사용하지 않는다. `DEV-010`, `DSH-008`, `DSH-010`, `DSH-024`

ArchiveCatalog의 마지막 기록 시각은 receipt 생성 시각이나 실행 중 시도가 아니라 현재 ArchiveRun의 1·2단계에서 성공 완료된 가장 최근 `archive_catalog` Step의 저장 시각을 사용한다. `DSH-027`

전체 작업 표면의 기본 다크 토큰은 검정보다 한 단계 밝은 어두운 회색 계층을 사용한다. Agent 대화 칸은 고른 테마의 작업 표면 색을 쓰는 둥근 패널로 본문에서 떨어져 있고, 사람의 지시는 오른쪽 말풍선, Agent 응답과 진행 한 줄은 왼쪽 글줄, 결정 대기 ActionCard는 둥근 카드, 처리된 카드는 한 줄 기록으로 둔다. `DSH-004`

화면은 같은 5초 projection을 읽되 응답 본문이 그대로면 다시 해석하지 않고, 바뀐 레코드(드라이브·폴더·작업·Job·Session)만 그 값을 읽는 행에 반영해 다시 그린다. 접힌 내용은 펼칠 때만 그리고, 200개가 넘는 폴더 표는 화면 근처의 행만 같은 높이로 그린다. 화면 이동은 깊이에 따른 방향으로, 작업의 목록 이동은 한 행의 이동으로 View Transition을 쓰며 줄인 움직임 설정에서는 투명도 변화만 남긴다. 화면 코드와 그 UI 라이브러리(Preact·signals·htm, `static/js/vendor/LICENSES.txt`)는 Control 바이너리에 내장해 제공하고 외부 네트워크를 쓰지 않으며, Content-Security-Policy의 `script-src 'self'`·`style-src 'self'`를 그대로 따른다. `DSH-004`, `DSH-007`, `DSH-019`

컴포넌트 화면은 기존 화면을 직접 대체한다. 별도 legacy route나 실행 중 전환 설정을 두지 않고 이전 화면 모듈도 배포하지 않는다. 루트 HTML은 mount point와 component entry만 가진 정적 shell이며 ArchiveRun·Job·Worker·Archivist projection이나 명령 입력값을 서버에서 직렬화해 넣지 않는다. 화면의 업무 상태 조회와 명령은 모두 같은 origin의 versioned `/api/v1` JSON API를 사용하고, health·정적 asset 같은 비업무 경로만 그 경계 밖에 둔다. Control의 Application Service와 Command Bus는 화면 모듈에서 직접 참조하지 않고 HTTP Adapter 뒤에 유지한다. stable asset URL은 직접 교체할 때 이전 모듈과 새 graph가 섞이지 않도록 root를 `no-store`, asset을 `no-cache, must-revalidate`로 제공한다. `DSH-034~035`

## 8. 초기 범위에서 제외

- 매체 소유권·대여·반환 업무
- 매체 반환 승인과 인수인계 완료 처리
- 원격 관리 설정 편집
- 브라우저에서 임의 shell 또는 임의 파일 작업 실행
- raw terminal 원격 조종과 Orca worktree·다중 terminal 관리
- 장기 로그 원문이나 영상 파일을 DB에 저장

실행 중 취소는 lease fence와 Worker 정리 확인까지 확정 계약이다. 완료된 효과의 조정과 업무 보상 후보는 [12. 취소와 보상 설계 제안](12-cancellation-compensation-proposal.md)에 분리한다.
