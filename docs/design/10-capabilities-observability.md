# 10. 도메인 기능 실행과 검증 증적

## 1. 독립 실행 단위

영속 업무 상태를 바꾸는 HTTP와 MCP 요청은 protocol 전용 타입에서 Application Service의 좁은 인터페이스로 변환한 뒤 같은 Command Bus에 제출한다. 모든 영속 업무를 한 Capability Registry에 등록해 Adapter 없이 직접 실행하는 목표 경계는 프로토타입만 있고 운영 조립에는 연결되지 않았으므로 `APP-001`은 planned다. 파일 변환 자체인 MediaOperation은 Command Bus와 DB가 필요한 업무 Command가 아니므로 별도 MediaOperation Port가 소유한다.

```text
HTTP ─┐
MCP ──┼→ Application Service Port → Command Bus + Domain Core
내부 조정기 ─┘

Capability Registry prototype ── planned ──┘
```

파일 처리 경계는 다음과 같다.

```text
archive-media CLI ─┐
관리 Runner ───────┼→ MediaOperation Port → FFmpeg·FFprobe·Whisper·StorageRoot
integration test ──┘
```

`archive-media`는 Control·대시보드·Worker 등록·service credential·SQLite 없이 typed operation 하나를 직접 실행한다. 관리 Runner는 같은 Port에 queue claim, ExecutionLease, 자원 관측과 결과 보고만 덧붙이는 선택형 Adapter다. 두 Adapter는 동시에 사용할 수 있으며 서로 다른 출력은 병렬 실행한다. 같은 Storage 출력은 Port가 StorageRoot의 교차 프로세스 잠금을 먼저 획득한 실행 하나만 Adapter에 진입시킨다. `APP-009~010`

목표 Capability는 versioned input, 필요한 Domain ID, 예상 리비전과 실행 이유를 받고 다음 둘 중 하나를 반환한다.

- 짧은 기능: Command receipt와 결과 ID
- 대용량 파일 기능: 생성된 Job ID, Step ID 또는 async task handle

목표 의존 방향에서 도메인 모듈은 서로의 Adapter를 호출하지 않고 합성은 Template의 Step 의존성과 Command Bus가 담당한다. 현재 Core는 이 규칙을 지키지만 `workerexecutor`, `workerclient`, `archiveorganization`, `archivisthostclient` Application 패키지는 구체 media·config Adapter를 직접 참조한다. 이 참조는 현재 실행 조립의 구조 부채이며 Core 계약을 바꾸지 않고 `cmd` 또는 Adapter 조립 경계로 이동해야 한다. MCP와 HTTP Adapter는 protocol 입력을 Application Service input으로 바꾸며, Archivist Host의 loopback MCP proxy는 ArchivistSession·ArchivistTurn·ArchivistToolCall 상관관계를 추가한다. `MCP-001~002`

대용량 파일 Job은 생성 시점의 업무 입력을 잃지 않는다. Coordinator는 Domain ID·Storage ID·정규화 상대경로·Profile reference를 Step별 typed `StepExecutionSpec`으로 내리고, 선행 출력이 필요한 Step의 명세는 이전 `StepOutputRecord`가 확정된 transaction에서 구체화한다. Capability receipt의 Job·Step handle과 실제 Worker 실행 입력은 이 두 기록으로 연결하며 protocol DTO나 Worker 로컬 절대경로를 Job 상태에 넣지 않는다. `APP-003~004`, `WRK-010~012`

Archive Control를 띄우지 않은 상태에서 영속 업무 Capability를 공통 Registry로 직접 호출하는 것은 `APP-001` planned 경계다. 현재 직접 실행이 보장되는 제품 기능은 Command Bus 바깥의 MediaOperation이며 `archive-media` CLI와 관리 Runner가 같은 구현을 호출한다. 요구사항: `APP-001`(planned), `APP-009~010`

## 2. 작업 공간 드라이브

대용량 파일 Capability는 임의 경로 대신 `storage_id`를 받는다. Storage Adapter는 Storage → Volume → Partition → PhysicalDevice 관계를 해석하고 각 입력·출력에 stable Volume ID인 `workspace_drive_id`와 별도 PhysicalDevice I/O 충돌 키를 붙인다.

- WorkspaceDrive 하나는 PhysicalDevice 하나다.
- 같은 Volume의 여러 Storage root는 같은 작업 공간이고 같은 PhysicalDevice의 서로 다른 Volume은 별도 작업 공간이다. PhysicalDevice lock은 두 작업 공간의 동시 heavy I/O를 계속 막는다.
- Job 하나가 여러 드라이브를 사용하면 각 PhysicalDevice를 별도로 예약하고 metric을 집계한다.
- scheduler, device lock과 안전 해제는 모두 같은 PhysicalDevice ID를 사용한다.
- 관계를 확인할 수 없는 Storage에서는 대용량 파일 Capability를 시작하지 않는다.

요구사항: `RES-006`, `DEV-006`

## 3. 실행 증적 경계

목표 경계에서 모니터링은 각 도메인 모듈에 로그 코드를 흩뿌리지 않고 Capability Executor를 감싸는 Evidence Sink가 담당한다. 현재 Evidence 타입·SQLite prototype은 있으나 Archive Control의 모든 실행 Port를 감싸지 않으므로 `OBS-001`은 planned다.

```text
CapabilityRequested
→ CapabilityStarted
→ progress / resource metric / process log reference
→ CapabilityCompleted | CapabilityFailed | CapabilityCancelled
```

각 기록은 최소한 다음 상관관계를 가진다.

```text
run_id, capability_name, capability_version
command_id, job_id, step_id, actor, requested_via
worker_id, archivist_host_id, archivist_session_id, archivist_turn_id, archivist_tool_call_id
workspace_drive_ids
started_at, observed_at, finished_at
outcome, error_class
input/output ID와 검증 증적 참조
```

Domain Event는 업무 상태 변경 사실이고, progress·metric·process log는 증적이다. 둘을 같은 것으로 취급하지 않는다. 완료·실패 상태와 CommandRecord·Event는 DB 원장에 남고, 고빈도 metric과 원문 로그는 회전 저장하며 DB에는 요약과 참조만 남긴다. 모든 Capability를 한 Evidence Sink로 묶는 요구사항은 `OBS-001` planned다.

공통 Evidence Sink 완성 전에도 진행 중 작업의 원인을 묻는 경로에는 현재 원장에 이미 있는 증적을 한 번에 읽는 `WorkDiagnostic` projection을 둔다. 이 projection은 Job·Step과 최신 Worker resource snapshot, 활성 ResourceReservation·ExecutionLease, 저장된 WaitReason·error class·안전한 log reference를 같은 관측 시각으로 결합한다. 실행 리스 heartbeat는 Worker 실행 루프가 살아 있다는 증적이지 subprocess가 계속 전진한다는 증적은 아니므로, 진행률 관측이 없는 장시간 RUNNING을 곧바로 실패나 정지로 바꾸지 않는다. 확인된 값은 증적 ID가 있는 `facts`, 아직 확인되지 않은 인과 설명은 예측·확인 방법과 증적 ID `basis`를 가진 `hypotheses`로 나눠 반환한다. `OBS-002`

`WorkDiagnostic`은 읽기 모델이며 StepState·WaitReason·ExecutionLease·ResourceReservation을 고치지 않는다. Worker heartbeat 만료, 실행 리스 누락·만료, 저장된 WAITING·FAILED, 새 progress 증적이 없는 장시간 RUNNING과 Control 예약·Worker 관측 load 불일치를 구조화된 진단 code와 다음 확인 방법으로 나타낸다. 장시간 STT는 청크 계획·시작·완료·실패와 원본 시간 범위를 `StepProgressEvidence`로 제공하므로 Archivist는 lease 생존과 실제 subprocess 전진을 구분한다. 원문 stdout·stderr, credential, 실행 인수와 절대경로는 진단 응답에 넣지 않는다. 작업 공간과 Archivist MCP에는 정규화 code·관측 시각·정제 summary·안전한 log reference만 제공한다. 고빈도 FFmpeg frame 진행과 정제된 log tail을 모든 실행기에 연결하는 일은 계속 `OBS-001`의 후속 범위다. `WRK-017`

작업 참조가 아직 없는 대화에서 전체 현황을 물을 때는 `SystemStatus` projection이 같은 WorkerCapacity·Archivist Host·관리 작업·WorkDiagnostic 읽기 모델을 조합한다. 이 projection은 Worker와 Host의 연결·최근 heartbeat, Worker의 예약 차감 자원과 추가 작업 가능 여부, 처리 중·시작 대기·직접 확인·실패·완료 작업 수를 반환한다. 아직 해결되지 않은 실패·직접 확인 작업과 진단상 보고가 필요한 처리 중 작업만 최근 순서로 최대 50건 포함하고, 처리된 과거 실패는 작업 공간과 같은 `replacement_work_id` 규칙으로 완료 이력에 센다. 각 문제는 정제된 진단과 증적 ID를 포함하지만 선택·WorkReference를 만들거나 업무 상태를 바꾸지 않는다. `MCP-012`

Worker가 보고한 각 실패는 `StepFailureEvidence`로 Job 원장에 남으므로 진단은 현재 오류뿐 아니라 이전 retry의 실패 순서·operation·Worker·fencing token·원인 code·process exit code까지 원래 증적 ID로 제공한다. Archivist는 이 ID가 붙은 항목만 확인된 사실로 설명하고, 기록에 없는 원인은 가설로 남겨 다음 확인 방법을 함께 제안한다. MCP 경계는 같은 정제 함수를 한 번 더 적용해 원문 process 데이터나 경로가 Worker 입력에 섞여도 외부로 내보내지 않는다. `WRK-016`, `OBS-003`, `MCP-009`, `MCP-012`

장시간 실행 중의 `StepProgressEvidence`도 같은 진단의 확인된 사실이다. 청크 상태·순번·전체 수·원본 millisecond 범위·진행률과 관측 시각만 제공하며, 청크 음성·전사문·process 원문은 포함하지 않는다. 실패한 청크는 마지막 진행 증적과 같은 fencing token의 `StepFailureEvidence`를 함께 읽어 어느 범위에서 실행이 멈췄는지 확인한다. `WRK-017`, `OBS-002`, `MCP-009`

Worker가 보고한 `ProcessLogArtifact` 메타데이터와 이중 정제된 제한 tail은 `StepFailureEvidence`와 같은 Unit of Work에 저장한다. 기본 `WorkDiagnostic`은 가용 상태와 실패 증적 ID만 제공하며 상세 tail은 펼침 조작이나 `archive.work.failure_evidence.get.v1`에서 해당 증적 ID를 지정했을 때만 반환한다. 과거의 `worker://operation-failed` 같은 일반 reference는 실제 산출물로 가장하지 않고 `LEGACY_UNRESOLVABLE`로 표시하며, 진단은 상세가 없는데 log 검사를 권하지 않는다. 원문은 Worker 로컬 보존 정책의 대상이고 Control DB·MCP·화면에는 들어오지 않는다. `OBS-004`, `MCP-013`, `DSH-045`

진단은 설명용 `suggested_actions`와 실행 가능한 `recovery_actions`를 구분한다. 후자는 Control이 현재 저장된 실행 경계를 확인해 안전하다고 판정한 만료 리스 조정 또는 일시적 아카이브 게시 종결 실패의 새 복구 Job만 Job revision·Step·선택적 reservation·fencing token·진단 fingerprint·basis 증적 ID와 함께 반환한다. 만료 리스 조정은 `StepRecoveryEvidence`를 추가하고, 종결 실패 복구는 원래 `FAILED`와 `StepFailureEvidence`를 그대로 둔 채 같은 실패 증적에서 한 번만 계산되는 Job ID와 `RecoverySource`를 저장한다. 복구 Job이 존재하면 진단은 `TERMINAL_RECOVERY_JOB_CREATED` 사실을 반환하고 같은 recovery action을 다시 내지 않는다. fingerprint나 실행 경계가 달라진 요청은 상태 변경 전에 거부한다. `WFL-014~015`, `WRK-018`, `MCP-011`

## 4. 요구사항 테스트와 증적 묶음

테스트는 private 구현 호출 횟수가 아니라 공개 Capability 또는 Core 계약의 상태, 오류, CommandRecord, Event와 Artifact를 관찰한다.

- Core unit test: 순수 규칙의 입력·결과를 검증하고 test runner가 결과를 증적으로 기록한다.
- Capability integration test: 공통 Registry가 운영에 연결된 뒤 실제 Application Port를 직접 실행하고 Evidence Sink에 성공 또는 실패 기록이 남는지 검증한다.
- Adapter integration test: HTTP, MCP, SQLite, filesystem 같은 실제 경계를 사용하되 같은 Capability 결과와 상관 ID를 검증한다.
- acceptance test: 실제 Windows 장치·GPU·FFmpeg 실행 결과와 장비 식별자를 증적에 포함한다.

모든 검증 테스트는 CI에서만 실행하고, CI job의 `evidence/<run_id>/`를 기계 판독 가능한 artifact로 업로드한다. 개발 PC의 로컬 테스트 결과는 완료 판정 증적으로 사용하지 않는다.

```text
run.json        CI run ID, 실행 시각, git revision, OS, toolchain, 전체 결과
results.jsonl   requirement_id, test 이름·수준, pass/fail, duration
go-test.jsonl   Go test의 원본 구조화 출력
artifacts/      manifest, checksum, 보고서 같은 작은 검증 산출물
```

대용량 fixture와 영상 출력은 복제하지 않고 checksum과 CI artifact 참조만 기록한다. 비밀값과 불필요한 원본 경로는 출력 전에 정제한다. 작업 디렉터리의 `evidence/`는 Git에서 제외하며 CI artifact retention을 적용한다. 장기 보존이 필요한 Windows acceptance 증적은 운영 아카이브로 승격한다.

개발 도구 자체의 테스트처럼 요구사항 ID가 없는 테스트도 결과를 기록하되 `requirement_id`를 비우고 `test_category=tooling`으로 구분한다. 실행된 테스트 하나가 `results.jsonl` 레코드 하나에 대응해야 한다. CI는 같은 revision에 대해 요구사항·온톨로지 검사, `go vet`, Linux test와 Windows amd64 cross-build도 수행한다. 요구사항: `TST-001`

운영 Windows 장치·GPU·FFmpeg acceptance는 현재 CI 성공과 분리한다. 운영기기가 SSH로 제공되면 별도 승인된 acceptance job이 장비 식별자와 결과를 같은 증적 형식으로 남긴다.

로컬 CLI acceptance는 실제 Windows Archivist Host·CLI ID·backend ID를 증적에 남기되 credential, resume token, raw 대화와 실제 filesystem path는 정제한다. 강제 연결 종료 전후의 ArchivistTurn event cursor, Tool Call receipt와 Command ID를 비교해 재개 뒤 중복 실행이 없음을 검증한다. `LRH-009`
