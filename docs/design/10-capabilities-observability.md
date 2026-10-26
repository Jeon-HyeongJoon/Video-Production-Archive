# 10. 도메인 기능 실행과 검증 증적

## 1. 독립 실행 단위

각 도메인 기능은 MCP tool이나 HTTP handler가 아니라 Application Capability로 먼저 구현한다.

```text
CLI / Test / Scheduler / HTTP / MCP
                 ↓
        Capability Registry
                 ↓
     Application Capability Port
                 ↓
       Command Bus + Domain Core
```

Capability는 versioned input, 필요한 Domain ID, 예상 리비전과 실행 이유를 받고 다음 둘 중 하나를 반환한다.

- 짧은 기능: Command receipt와 결과 ID
- 대용량 파일 기능: 생성된 Job ID, Step ID 또는 async task handle

도메인 모듈은 서로의 Adapter를 호출하지 않는다. 합성은 Template의 Step 의존성과 Command Bus가 담당한다. MCP, HTTP와 CLI Adapter는 protocol 입력을 같은 Capability input으로 바꾸고 결과를 protocol response로 변환할 뿐 업무 규칙을 구현하지 않는다.

Control Service를 띄우지 않은 integration test와 진단 CLI에서도 같은 Capability를 직접 호출할 수 있어야 한다. 요구사항: `APP-001`

## 2. 작업 공간 드라이브

대용량 파일 Capability는 임의 경로 대신 `storage_id`를 받는다. Storage Adapter는 Storage → Volume → Partition → PhysicalDevice 관계를 해석하고 각 입력·출력에 `workspace_drive_id`를 붙인다.

- WorkspaceDrive 하나는 PhysicalDevice 하나다.
- 같은 PhysicalDevice에 속한 여러 Volume이나 Storage root는 같은 작업 공간으로 예약한다.
- Job 하나가 여러 드라이브를 사용하면 각 PhysicalDevice를 별도로 예약하고 metric을 집계한다.
- scheduler, device lock과 안전 해제는 모두 같은 PhysicalDevice ID를 사용한다.
- 관계를 확인할 수 없는 Storage에서는 대용량 파일 Capability를 시작하지 않는다.

요구사항: `RES-006`, `DEV-006`

## 3. 실행 증적 경계

모니터링은 각 도메인 모듈에 로그 코드를 흩뿌리지 않고 Capability Executor를 감싸는 Evidence Sink가 담당한다.

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
worker_id, workspace_drive_ids
started_at, observed_at, finished_at
outcome, error_class
input/output ID와 검증 증적 참조
```

Domain Event는 업무 상태 변경 사실이고, progress·metric·process log는 증적이다. 둘을 같은 것으로 취급하지 않는다. 완료·실패 상태와 CommandRecord·Event는 DB 원장에 남고, 고빈도 metric과 원문 로그는 회전 저장하며 DB에는 요약과 참조만 남긴다. 요구사항: `OBS-001`

## 4. 요구사항 테스트와 증적 묶음

테스트는 private 구현 호출 횟수가 아니라 공개 Capability 또는 Core 계약의 상태, 오류, CommandRecord, Event와 Artifact를 관찰한다.

- Core unit test: 순수 규칙의 입력·결과를 검증하고 test runner가 결과를 증적으로 기록한다.
- Capability integration test: 실제 Application Port를 직접 실행하고 Evidence Sink에 성공 또는 실패 기록이 남는지 검증한다.
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
